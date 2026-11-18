package kvm

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Resources of a VM when the caller names none.
const (
	defaultCPUs     = 2
	defaultMemoryMB = 4096
)

// firmwares are where distributions put the UEFI firmware for arm64 guests and the template
// for its variables: Debian and Ubuntu (qemu-efi-aarch64), then Fedora (edk2-aarch64).
var firmwares = [][2]string{
	{"/usr/share/AAVMF/AAVMF_CODE.fd", "/usr/share/AAVMF/AAVMF_VARS.fd"},
	{"/usr/share/edk2/aarch64/QEMU_EFI-pflash.raw", "/usr/share/edk2/aarch64/vars-template-pflash.raw"},
}

// firmware is the UEFI firmware to boot with: its code and the template for its variables.
// x86_64 guests boot with the BIOS built into QEMU and need none.
func (k *KVM) firmware() ([2]string, error) {
	if k.Arch != "arm64" {
		return [2]string{}, nil
	}
	for _, f := range firmwares {
		if _, err := os.Stat(f[0]); err == nil {
			if _, err := os.Stat(f[1]); err == nil {
				return f, nil
			}
		}
	}
	return [2]string{}, errors.New("the UEFI firmware for arm64 guests is not installed (package qemu-efi-aarch64 or edk2-aarch64)")
}

// qemuArgs is the command line for a VM. Everything in it comes from the image's config, the
// owner's policy and paths the executor made; nothing from a job.
func (k *KVM) qemuArgs(s bootSpec, m *machine, publicKey string, shares []share) ([]string, error) {
	cpus, memory := s.resources.CPUs, s.resources.MemoryMB
	if cpus <= 0 {
		cpus = defaultCPUs
	}
	if memory <= 0 {
		memory = defaultMemoryMB
	}
	// Huge pages come in twos of megabytes.
	memory -= memory % 2

	machine := "q35,accel=kvm"
	if k.Arch == "arm64" {
		machine = "virt,accel=kvm,gic-version=host"
	}
	args := []string{
		"-name", s.name,
		"-nodefaults", "-no-user-config", "-display", "none",
		"-cpu", "host",
		"-smp", fmt.Sprint(cpus),
		"-m", fmt.Sprintf("%dM", memory),
	}

	// virtiofs needs the guest's memory to be shared with the daemon that serves the files.
	shared := false
	for _, sh := range shares {
		shared = shared || sh.socket != ""
	}
	if shared {
		backend := fmt.Sprintf("memory-backend-memfd,id=mem,size=%dM,share=on", memory)
		// Reserved huge pages, where the owner has set enough aside, make shared memory much
		// cheaper; under nested virtualization it is unusably slow without them.
		if hugePagesFreeMB() >= memory {
			backend += ",hugetlb=on"
		}
		args = append(args, "-object", backend)
		machine += ",memory-backend=mem"
	}
	args = append(args, "-machine", machine)

	fw, err := k.firmware()
	if err != nil {
		return nil, err
	}
	if fw[0] != "" {
		vars := filepath.Join(m.dir, "vars.fd")
		if err := copyFile(fw[1], vars); err != nil {
			return nil, err
		}
		args = append(args,
			"-drive", "if=pflash,format=raw,readonly=on,file="+comma(fw[0]),
			"-drive", "if=pflash,format=raw,file="+comma(vars))
	}

	root := "if=none,id=root,format=qcow2,discard=unmap,file=" + comma(s.disk)
	if s.disposable {
		root += ",cache=unsafe"
	}
	args = append(args, "-drive", root, "-device", "virtio-blk-pci,drive=root,bootindex=1")
	for i, sh := range shares {
		if sh.socket != "" {
			args = append(args,
				"-chardev", fmt.Sprintf("socket,id=fs%d,path=%s", i, comma(sh.socket)),
				"-device", fmt.Sprintf("vhost-user-fs-pci,chardev=fs%d,tag=%s", i, sh.name))
		} else {
			args = append(args,
				"-drive", fmt.Sprintf("if=none,id=ro%d,format=raw,readonly=on,file=%s", i, comma(sh.disk)),
				"-device", fmt.Sprintf("virtio-blk-pci,drive=ro%d,serial=%s", i, sh.serial))
		}
	}
	args = append(args,
		// User-mode networking needs no privileges; the guest's SSH port appears on the host's
		// loopback address only.
		"-netdev", fmt.Sprintf("user,id=net0,hostfwd=tcp:127.0.0.1:%d-:22", m.port),
		"-device", "virtio-net-pci,netdev=net0,romfile=",
		"-device", "virtio-rng-pci",
		// systemd in the guest turns this SMBIOS string into a credential, which the image's
		// mcc-guest-access service installs as the job user's only authorized key.
		"-smbios", "type=11,value="+comma("io.systemd.credential:mcc.authorized_key="+publicKey),
		"-serial", "file:"+comma(filepath.Join(m.dir, "console.log")),
		"-qmp", "unix:"+comma(filepath.Join(m.dir, "qmp.sock"))+",server=on,wait=off",
		"-pidfile", filepath.Join(m.dir, "qemu.pid"),
	)
	return args, nil
}

// comma escapes a value inside a QEMU option, where a comma separates settings.
func comma(s string) string {
	return strings.ReplaceAll(s, ",", ",,")
}

// hugePagesFreeMB is the memory in huge pages that the host has reserved and nothing uses.
func hugePagesFreeMB() int {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	var free, sizeKB int
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		_, _ = fmt.Sscanf(sc.Text(), "HugePages_Free: %d", &free)
		_, _ = fmt.Sscanf(sc.Text(), "Hugepagesize: %d kB", &sizeKB)
	}
	return free * sizeKB / 1024
}

// HostMemoryMB is the host's memory, or 0 where it cannot be read.
func HostMemoryMB() int {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var kb int
		if n, _ := fmt.Sscanf(sc.Text(), "MemTotal: %d kB", &kb); n == 1 {
			return kb / 1024
		}
	}
	return 0
}

// qmp sends commands to a QEMU's control socket, which reaches exactly the VM whose directory
// the socket is in.
func qmp(socket string, commands ...string) error {
	conn, err := net.DialTimeout("unix", socket, 3*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	dec := json.NewDecoder(conn)
	var msg map[string]json.RawMessage
	if err := dec.Decode(&msg); err != nil { // the greeting
		return err
	}
	for _, c := range append([]string{"qmp_capabilities"}, commands...) {
		if _, err := fmt.Fprintf(conn, `{"execute":%q}`+"\n", c); err != nil {
			return err
		}
		for {
			msg = nil
			if err := dec.Decode(&msg); err != nil {
				// QEMU closes the socket as it quits.
				if c == "quit" {
					return nil
				}
				return err
			}
			if e, ok := msg["error"]; ok {
				return fmt.Errorf("qemu refused %s: %s", c, e)
			}
			if _, ok := msg["return"]; ok {
				break
			}
		}
	}
	return nil
}
