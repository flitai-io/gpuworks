//go:build linux

package terminal

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestOtherDevptsHolderHelper is not a test: it is the process
// TestSessionCloseSparesTheSameNumberOnAnotherDevpts starts in a user and mount
// namespace of its own. It mounts a private devpts, as a container does, takes
// the pts number named by GPUWORKS_TERMINAL_DEVPTS_NUMBER there, and holds that
// slave on stdin until it is killed.
func TestOtherDevptsHolderHelper(t *testing.T) {
	want := os.Getenv("GPUWORKS_TERMINAL_DEVPTS_NUMBER")
	if want == "" {
		t.Skip("helper process only")
	}
	fail := func(format string, args ...any) {
		fmt.Printf("skip: "+format+"\n", args...)
		os.Exit(0)
	}
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		fail("make mounts private: %v", err)
	}
	if err := syscall.Mount("devpts", "/dev/pts", "devpts", 0, "newinstance,ptmxmode=0666"); err != nil {
		fail("mount devpts: %v", err)
	}
	number, _ := strconv.Atoi(want)
	for {
		master, err := unix.Open("/dev/pts/ptmx", unix.O_RDWR|unix.O_NOCTTY, 0)
		if err != nil {
			fail("open ptmx: %v", err)
		}
		if err = unix.IoctlSetPointerInt(master, unix.TIOCSPTLCK, 0); err != nil {
			fail("unlock: %v", err)
		}
		n, err := unix.IoctlGetInt(master, unix.TIOCGPTN)
		if err != nil {
			fail("pts number: %v", err)
		}
		if n < number {
			continue // keep this master open so the next one gets n+1
		}
		if n > number {
			fail("got pts %d past %d", n, number)
		}
		slave, err := unix.Open(fmt.Sprintf("/dev/pts/%d", n), unix.O_RDWR|unix.O_NOCTTY, 0)
		if err != nil {
			fail("open slave: %v", err)
		}
		if err = unix.Dup2(slave, 0); err != nil {
			fail("dup2: %v", err)
		}
		fmt.Println("ready")
		time.Sleep(time.Minute)
		os.Exit(0)
	}
}

// A process in a container that has its own /dev/pts names its terminal by the
// same path as an agent session on the host with the same number. Closing the
// agent's session must leave it alone.
func TestSessionCloseSparesTheSameNumberOnAnotherDevpts(t *testing.T) {
	s, err := Open(Spec{Argv: []string{"sleep", "300"}, Grace: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	number := strings.TrimPrefix(s.tty.path, "/dev/pts/")
	if _, err := strconv.Atoi(number); err != nil {
		t.Fatalf("session tty %q is not a /dev/pts slave", s.tty.path)
	}

	helper := exec.Command(os.Args[0], "-test.run=^TestOtherDevptsHolderHelper$")
	helper.Env = append(os.Environ(), "GPUWORKS_TERMINAL_DEVPTS_NUMBER="+number)
	helper.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}
	out, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = helper.Start(); err != nil {
		t.Skipf("user namespaces unavailable, cannot give the holder its own devpts: %v", err)
	}
	defer func() {
		_ = helper.Process.Kill()
		_ = helper.Wait()
	}()
	line, err := bufio.NewReader(out).ReadString('\n')
	if strings.HasPrefix(line, "skip: ") {
		t.Skipf("cannot mount a devpts of its own: %s", strings.TrimPrefix(line, "skip: "))
	}
	if err != nil || strings.TrimSpace(line) != "ready" {
		t.Fatalf("holder did not get ready: %q %v", line, err)
	}
	holder := helper.Process.Pid
	// The premise: by path the holder is indistinguishable from the session.
	if target, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/0", holder)); err != nil || target != s.tty.path {
		t.Fatalf("holder stdin = %q (%v), want the same path as the session's %s", target, err, s.tty)
	}
	for _, p := range processesUsingTTY(s.tty) {
		if p.pid == holder {
			t.Fatalf("holder on another devpts counted as using %s", s.tty)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(holder, 0); err != nil {
		t.Fatalf("closing the session killed the holder on another devpts: %v", err)
	}
}

// A process that holds the session's slave is found both before and after the
// master closes, when /proc shows its link as "/dev/pts/N (deleted)". While
// pinned, the number is not handed to a new terminal even once that holder is
// gone too.
func TestProcessesUsingTTYFollowsTheSlaveThroughMasterClose(t *testing.T) {
	p, err := openPTY()
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	ref := ttyRefOf(p.slave)
	holder := exec.Command("sleep", "300")
	holder.Stdin = p.slave
	if err = holder.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := false
	defer func() {
		if !reaped {
			_ = holder.Process.Kill()
			_ = holder.Wait()
		}
	}()
	found := func() bool {
		for _, process := range processesUsingTTY(ref) {
			if process.pid == holder.Process.Pid {
				return true
			}
		}
		return false
	}
	if !found() {
		t.Fatalf("holder %d of %s not found", holder.Process.Pid, ref.path)
	}
	pin := pinTTY(ref)
	if pin == nil {
		t.Fatalf("pin %s", ref.path)
	}
	defer pin.Close()
	p.close()
	if link, _ := os.Readlink(fmt.Sprintf("/proc/%d/fd/0", holder.Process.Pid)); link != ref.path+" (deleted)" {
		t.Fatalf("holder link after master close = %q", link)
	}
	if !found() {
		t.Fatalf("holder %d not found once the master closed", holder.Process.Pid)
	}
	_ = holder.Process.Kill()
	_ = holder.Wait()
	reaped = true
	var opened []*pty
	defer func() {
		for _, q := range opened {
			q.close()
		}
	}()
	for range 4 {
		q, err := openPTY()
		if err != nil {
			t.Fatal(err)
		}
		opened = append(opened, q)
		if q.slave.Name() == ref.path {
			t.Fatalf("pinned %s handed to a new terminal", ref.path)
		}
	}
}
