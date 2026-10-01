//go:build linux

package fence

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// Landlock system calls: the same numbers on every architecture.
const (
	sysCreateRuleset = 444
	sysAddRule       = 445
	sysRestrictSelf  = 446

	createRulesetVersion = 1 << 0
	ruleTypePathBeneath  = 1
	prSetNoNewPrivs      = 38
	oPath                = 0x200000
)

const (
	accExecute    = 1 << 0
	accWriteFile  = 1 << 1
	accReadFile   = 1 << 2
	accReadDir    = 1 << 3
	accRemoveDir  = 1 << 4
	accRemoveFile = 1 << 5
	accMakeChar   = 1 << 6
	accMakeDir    = 1 << 7
	accMakeReg    = 1 << 8
	accMakeSock   = 1 << 9
	accMakeFifo   = 1 << 10
	accMakeBlock  = 1 << 11
	accMakeSym    = 1 << 12
	accRefer      = 1 << 13
	accTruncate   = 1 << 14
	accIoctlDev   = 1 << 15
	accResolveUnx = 1 << 16

	scopeAbstractUnix = 1 << 0
	scopeSignal       = 1 << 1

	sysCloseRange     = 436
	closeRangeCloexec = 1 << 2
)

// MinABI is the oldest Landlock ABI that keeps every file deny: ABI 3
// adds the truncate right, and a ruleset that leaves truncate unhandled
// lets a process empty any file it can open.
const MinABI = 3

// IPCABI is the ABI that also stops unix-socket connects outside the
// granted socket dirs (RESOLVE_UNIX, ABI 9). ScopeABI stops abstract unix
// sockets and signals to processes outside the fence (ABI 6). Below
// either, the fence refuses unless the caller allows weak IPC (FN-R-10).
const (
	IPCABI   = 9
	ScopeABI = 6
)

// Probe asks the kernel for its Landlock ABI. A test replaces it.
var Probe = kernelABI

// ErrNoLandlock says why the kernel cannot fence.
type ErrNoLandlock struct{ Why string }

func (e *ErrNoLandlock) Error() string { return e.Why }

// kernelABI asks the kernel. DAISUGI_FENCE_PROBE can only make the answer
// worse (enosys, eopnotsupp, abi:N below the real one), so a test can drive
// the refusal paths and nothing can use it to fake a fence.
func kernelABI() (int, error) {
	abi, err := askKernel()
	if err != nil {
		return 0, err
	}
	switch v := os.Getenv("DAISUGI_FENCE_PROBE"); {
	case v == "enosys":
		return 0, &ErrNoLandlock{"this kernel was built without Landlock"}
	case v == "eopnotsupp":
		return 0, &ErrNoLandlock{"Landlock is built into this kernel but switched off at boot (add landlock to lsm=)"}
	case strings.HasPrefix(v, "abi:"):
		if n, e := strconv.Atoi(v[4:]); e == nil && n < abi {
			return n, nil
		}
	}
	return abi, nil
}

func askKernel() (int, error) {
	r, _, errno := syscall.Syscall(sysCreateRuleset, 0, 0, createRulesetVersion)
	switch errno {
	case 0:
		return int(r), nil
	case syscall.ENOSYS:
		return 0, &ErrNoLandlock{"this kernel was built without Landlock"}
	case syscall.EOPNOTSUPP:
		return 0, &ErrNoLandlock{"Landlock is built into this kernel but switched off at boot (add landlock to lsm=)"}
	}
	return 0, &ErrNoLandlock{"the kernel refused to report its Landlock version: " + errno.Error()}
}

// Check is the probe and the floor: the ABI to fence with, or why not.
// weakIPC lets a kernel below IPCABI or ScopeABI fence the files only.
func Check(weakIPC bool) (int, error) {
	abi, err := Probe()
	if err != nil {
		return 0, err
	}
	if abi < MinABI {
		return 0, &ErrNoLandlock{fmt.Sprintf("this kernel has Landlock ABI %d and the fence needs ABI %d or newer "+
			"(it must stop truncate, which older kernels cannot)", abi, MinABI)}
	}
	if abi < IPCABI && !weakIPC {
		return 0, &ErrNoLandlock{fmt.Sprintf("this kernel has Landlock ABI %d and the fence needs ABI %d to stop "+
			"connects to unix sockets (the session bus, ssh-agent) and ABI %d to stop signals; "+
			"--allow-weak-ipc fences the files only", abi, IPCABI, ScopeABI)}
	}
	return abi, nil
}

// handled is every file-system right the kernel at this ABI knows, the
// unix-socket connect right among them from ABI 9, and no network right
// (FN-5).
func handled(abi int) uint64 {
	h := uint64(accExecute | accWriteFile | accReadFile | accReadDir | accRemoveDir | accRemoveFile |
		accMakeChar | accMakeDir | accMakeReg | accMakeSock | accMakeFifo | accMakeBlock | accMakeSym |
		accRefer | accTruncate)
	if abi >= 5 {
		h |= accIoctlDev
	}
	if abi >= IPCABI {
		h |= accResolveUnx
	}
	return h
}

// scoped is the scope set: abstract unix sockets and signals stay inside
// the fence from ABI 6.
func scoped(abi int) uint64 {
	if abi >= ScopeABI {
		return scopeAbstractUnix | scopeSignal
	}
	return 0
}

var fileRights = uint64(accExecute | accWriteFile | accReadFile | accTruncate | accIoctlDev)

const roRights = accExecute | accReadFile | accReadDir

// rights is the mask a rule gets: ro is read and execute, rw is every
// right but the socket connect, unix is only that, sock and rw+unix add it
// to ro and rw. A file target takes only the rights a file can have.
func rights(mode string, isDir bool, h uint64) uint64 {
	var r uint64
	switch mode {
	case "rw":
		r = h &^ accResolveUnx
	case "rw+unix":
		r = h
	case "unix":
		r = accResolveUnx
	case "sock":
		r = roRights | accResolveUnx
	default:
		r = roRights
	}
	if !isDir {
		r &= fileRights
	}
	return r & h
}

// writable says whether a mode can change what it covers.
func writable(mode string) bool { return mode == "rw" || mode == "rw+unix" }

// Applied says what Apply did.
type Applied struct {
	ABI     int
	Granted int
	Skipped []string
}

// Apply sets no_new_privs, builds the ruleset from rules, restricts the
// calling thread and, through it, every process it starts. The caller must
// have locked its OS thread and must exec from that same thread. guard, when
// set, checks the real path each rule opened against the plan's protected
// paths: a rule whose root became a link to a secret is refused (FN-R-16).
func Apply(rules []Rule, weakIPC bool, guard *Guard) (*Applied, error) {
	abi, err := Check(weakIPC)
	if err != nil {
		return nil, err
	}
	h := handled(abi)
	if _, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); e != 0 {
		return nil, fmt.Errorf("no_new_privs: %w", e)
	}
	// struct landlock_ruleset_attr: handled_access_fs, handled_access_net
	// (0: FN-5) and scoped. A kernel before ABI 6 knows only the first.
	attr := make([]byte, 24)
	binary.LittleEndian.PutUint64(attr, h)
	binary.LittleEndian.PutUint64(attr[16:], scoped(abi))
	size := 24
	if abi < ScopeABI {
		size = 8
	}
	fd, _, e := syscall.Syscall(sysCreateRuleset, uintptr(unsafe.Pointer(&attr[0])), uintptr(size), 0)
	if e != 0 {
		return nil, fmt.Errorf("landlock_create_ruleset: %w", e)
	}
	rs := int(fd)
	defer syscall.Close(rs)
	out := &Applied{ABI: abi}
	for _, r := range rules {
		pf, err := syscall.Open(r.Path, oPath|syscall.O_CLOEXEC, 0)
		if err != nil {
			out.Skipped = append(out.Skipped, r.Path)
			continue
		}
		var st syscall.Stat_t
		if err := syscall.Fstat(pf, &st); err != nil {
			syscall.Close(pf)
			out.Skipped = append(out.Skipped, r.Path)
			continue
		}
		if guard != nil {
			if why := guard.Opened(r, pf); why != "" {
				syscall.Close(pf)
				return nil, &ErrRefused{why}
			}
		}
		isDir := st.Mode&syscall.S_IFMT == syscall.S_IFDIR
		mask := rights(r.Mode, isDir, h)
		if mask == 0 {
			// The connect right on a kernel before ABI 9 (weak IPC allowed).
			syscall.Close(pf)
			out.Skipped = append(out.Skipped, r.Path)
			continue
		}
		buf := make([]byte, 12) // struct landlock_path_beneath_attr is packed
		binary.LittleEndian.PutUint64(buf, mask)
		binary.LittleEndian.PutUint32(buf[8:], uint32(pf))
		_, _, e := syscall.Syscall6(sysAddRule, uintptr(rs), ruleTypePathBeneath, uintptr(unsafe.Pointer(&buf[0])), 0, 0, 0)
		syscall.Close(pf)
		if e != 0 {
			return nil, fmt.Errorf("landlock_add_rule %s: %w", r.Path, e)
		}
		out.Granted++
	}
	if _, _, e := syscall.RawSyscall(sysRestrictSelf, uintptr(rs), 0, 0); e != 0 {
		return nil, fmt.Errorf("landlock_restrict_self: %w", e)
	}
	return out, nil
}

// ErrRefused is a rule the guard refused at apply time.
type ErrRefused struct{ Why string }

func (e *ErrRefused) Error() string { return e.Why }

// Opened is the guard's check on an opened rule root: it reads the path
// the kernel resolved (/proc/self/fd/N) and, when it differs from the
// rule's spelling, checks it again as the plan did.
func (g *Guard) Opened(r Rule, fd int) string {
	real, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
	if err != nil {
		return "cannot read where " + r.Path + " resolves: " + err.Error()
	}
	if real == r.Path {
		return ""
	}
	return g.Check(real, r.Mode, r.Path)
}

// CloseInherited marks every descriptor from 3 up close-on-exec, then
// clears the mark on each kept one: a file or socket the fence's caller
// left open is not carried past the exec, since Landlock checks only at
// open time (FN-R-18).
func CloseInherited(keep []int) error {
	if _, _, e := syscall.RawSyscall(sysCloseRange, 3, ^uintptr(0)>>32, closeRangeCloexec); e != 0 {
		if e != syscall.ENOSYS && e != syscall.EINVAL {
			return fmt.Errorf("close_range: %w", e)
		}
		ents, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			return err
		}
		for _, ent := range ents {
			if n, err := strconv.Atoi(ent.Name()); err == nil && n >= 3 {
				syscall.CloseOnExec(n)
			}
		}
	}
	for _, fd := range keep {
		if _, _, e := syscall.RawSyscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_SETFD, 0); e != 0 {
			return fmt.Errorf("--keep-fd %d: %w", fd, e)
		}
	}
	return nil
}

// Exec applies rules on this thread, then replaces the process with argv.
// It returns only on failure. argv[0] is a path to run.
func Exec(rules []Rule, weakIPC bool, guard *Guard, keep []int, argv0 string, argv, environ []string) (*Applied, error) {
	runtime.LockOSThread()
	ap, err := Apply(rules, weakIPC, guard)
	if err != nil {
		return nil, err
	}
	if err := CloseInherited(keep); err != nil {
		return ap, err
	}
	err = syscall.Exec(argv0, argv, environ)
	if errors.Is(err, syscall.EACCES) {
		err = fmt.Errorf("%w (the fence may hide the program or the file it points to; add its directory with --allow)", err)
	}
	return ap, err
}
