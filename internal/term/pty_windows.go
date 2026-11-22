//go:build windows

// Windows backend: ConPTY when available (Windows 10 1809+ / Server 2019+),
// plain pipe mode as a fallback on older systems (no ANSI output).
package term

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// PTY is one shell session pseudoterminal.
type PTY interface {
	io.ReadWriteCloser
	Resize(cols, rows uint16) error
	Kill() error
	Wait() (int, error)
}

var (
	modkernel32             = windows.NewLazySystemDLL("kernel32.dll")
	procCreatePseudoConsole = modkernel32.NewProc("CreatePseudoConsole")
	procResizePseudoConsole = modkernel32.NewProc("ResizePseudoConsole")
	procClosePseudoConsole  = modkernel32.NewProc("ClosePseudoConsole")
	procInitAttrList        = modkernel32.NewProc("InitializeProcThreadAttributeList")
	procUpdateAttr          = modkernel32.NewProc("UpdateProcThreadAttribute")
	procDeleteAttrList      = modkernel32.NewProc("DeleteProcThreadAttributeList")
	procCreateJobObject     = modkernel32.NewProc("CreateJobObjectW")
	procSetJobInfo          = modkernel32.NewProc("SetInformationJobObject")
	procAssignToJob         = modkernel32.NewProc("AssignProcessToJobObject")
	procTerminateJob        = modkernel32.NewProc("TerminateJobObject")
)

// Job-object plumbing for killing the whole shell tree: a ConPTY session must
// not leak grandchildren when the shell or the daemon goes away. Local copies
// of the Win32 limit structs (field order matches winnt.h; Go's natural
// alignment reproduces the C padding on 64-bit platforms).
const (
	jobObjectExtendedLimitInfo = 9      // JobObjectExtendedLimitInformation
	jobLimitKillOnJobClose     = 0x2000 // JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
)

type jobBasicLimitInfo struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type jobIOCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobExtendedLimitInfo struct {
	BasicLimitInformation jobBasicLimitInfo
	IoInfo                jobIOCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

func conptyAvailable() bool { return procCreatePseudoConsole.Find() == nil }

// DefaultShell picks the interactive shell.
func DefaultShell() string {
	if p, err := exec.LookPath("pwsh.exe"); err == nil {
		return p
	}
	if p, err := exec.LookPath("powershell.exe"); err == nil {
		return p
	}
	return filepath.Join(os.Getenv("WINDIR"), "system32", "cmd.exe")
}

// shellArgs tunes shell startup: quiet banner + UTF-8 codepage where the
// shell supports it (mostly helps the legacy pipe fallback on old Windows;
// ConPTY handles UTF-8 itself).
func shellArgs(shell string) []string {
	base := strings.ToLower(filepath.Base(shell))
	switch {
	case strings.Contains(base, "cmd"):
		return []string{"/k", "chcp", "65001", ">nul"}
	case strings.Contains(base, "powershell"), strings.Contains(base, "pwsh"):
		return []string{"-NoLogo"}
	default:
		return nil
	}
}

// quoteCmdToken wraps a command-line token in quotes when it contains
// whitespace, per the standard CRT argv parsing rules.
func quoteCmdToken(s string) string {
	if strings.ContainsAny(s, " \t") {
		return `"` + s + `"`
	}
	return s
}

// StartPTY launches a shell session.
func StartPTY(cols, rows uint16, shell, cwd string) (PTY, error) {
	if shell == "" {
		shell = DefaultShell()
	}
	if conptyAvailable() {
		return startConPTY(cols, rows, shell, cwd)
	}
	return startPipePTY(shell, cwd)
}

// ---------------------------------------------------------------- ConPTY ---

type coord struct{ X, Y int16 }

type startupInfoEx struct {
	windows.StartupInfo
	AttributeList *byte
}

const (
	extendedStartupInfoPresent  = 0x00080000
	createUnicodeEnvironment    = 0x00000400
	procThreadAttrPseudoconsole = 0x00020016
)

type conPTY struct {
	inW  *os.File
	outR *os.File
	hpc  windows.Handle
	hPrc windows.Handle
	hJob windows.Handle // tree-kill handle; 0 when the job could not be created

	mu     sync.Mutex
	code   int
	exited bool // Wait ran and tore the handles down; Kill/Resize are no-ops
}

func startConPTY(cols, rows uint16, shell, cwd string) (PTY, error) {
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		_ = inR.Close()
		_ = inW.Close()
		return nil, err
	}
	var hpc windows.Handle
	size := coord{X: int16(cols), Y: int16(rows)}
	r1, _, callErr := procCreatePseudoConsole.Call(
		uintptr(*(*uint32)(unsafe.Pointer(&size))),
		uintptr(windows.Handle(inR.Fd())),
		uintptr(windows.Handle(outW.Fd())),
		0,
		uintptr(unsafe.Pointer(&hpc)),
	)
	if r1 != 0 { // HRESULT: nonzero = failure
		for _, f := range []*os.File{inR, inW, outR, outW} {
			_ = f.Close()
		}
		return nil, fmt.Errorf("CreatePseudoConsole: %v", callErr)
	}
	// The pseudoconsole owns its copies; close our ends of what we handed it.
	_ = inR.Close()
	_ = outW.Close()

	// Attribute list with one entry. The sizing call is expected to fail with
	// ERROR_INSUFFICIENT_BUFFER while writing the required size; a zero size
	// means it failed outright and would panic on &attr[0] below.
	var listSize uintptr
	_, _, _ = procInitAttrList.Call(0, 1, 0, uintptr(unsafe.Pointer(&listSize)))
	if listSize == 0 {
		_, _, _ = procClosePseudoConsole.Call(uintptr(hpc))
		_ = inW.Close()
		_ = outR.Close()
		return nil, fmt.Errorf("InitializeProcThreadAttributeList: could not size attribute list")
	}
	attr := make([]byte, listSize)
	r1, _, callErr = procInitAttrList.Call(uintptr(unsafe.Pointer(&attr[0])), 1, 0, uintptr(unsafe.Pointer(&listSize)))
	if r1 == 0 {
		_, _, _ = procClosePseudoConsole.Call(uintptr(hpc))
		_ = inW.Close()
		_ = outR.Close()
		return nil, fmt.Errorf("InitializeProcThreadAttributeList: %v", callErr)
	}
	defer procDeleteAttrList.Call(uintptr(unsafe.Pointer(&attr[0])))
	r1, _, callErr = procUpdateAttr.Call(
		uintptr(unsafe.Pointer(&attr[0])), 0, procThreadAttrPseudoconsole,
		uintptr(hpc), unsafe.Sizeof(hpc), 0, 0,
	)
	if r1 == 0 {
		_, _, _ = procClosePseudoConsole.Call(uintptr(hpc))
		_ = inW.Close()
		_ = outR.Close()
		return nil, fmt.Errorf("UpdateProcThreadAttribute: %v", callErr)
	}

	siEx := startupInfoEx{}
	siEx.Cb = uint32(unsafe.Sizeof(siEx))
	siEx.AttributeList = &attr[0]
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	cwdPtr, err := windows.UTF16PtrFromString(cwd)
	if err != nil {
		_, _, _ = procClosePseudoConsole.Call(uintptr(hpc))
		_ = inW.Close()
		_ = outR.Close()
		return nil, err
	}
	shellPtr, _ := windows.UTF16PtrFromString(shell)
	// Quote the program path when it contains spaces (pwsh installs under
	// "C:\Program Files\...") so the child's CRT parses argv[0] correctly;
	// lpApplicationName gets the exact path, but lpCommandLine is what the
	// shell's argument parser sees. shellArgs tokens are fixed and simple.
	cmdline := quoteCmdToken(shell)
	if args := shellArgs(shell); len(args) > 0 {
		cmdline += " " + strings.Join(args, " ")
	}
	cmdlinePtr, _ := windows.UTF16PtrFromString(cmdline)
	var pi windows.ProcessInformation
	err = windows.CreateProcess(
		shellPtr, cmdlinePtr, nil, nil, false,
		extendedStartupInfoPresent|createUnicodeEnvironment,
		nil, cwdPtr, &siEx.StartupInfo, &pi,
	)
	if err != nil {
		_, _, _ = procClosePseudoConsole.Call(uintptr(hpc))
		_ = inW.Close()
		_ = outR.Close()
		return nil, fmt.Errorf("CreateProcess %s: %w", shell, err)
	}
	_ = windows.CloseHandle(pi.Thread)
	// Put the shell into a kill-on-close job so its children die with it;
	// without the job, Kill can only terminate the shell itself. Best
	// effort: job creation failing must not fail the whole PTY.
	hJob, _, _ := procCreateJobObject.Call(0, 0)
	if hJob != 0 {
		var info jobExtendedLimitInfo
		info.BasicLimitInformation.LimitFlags = jobLimitKillOnJobClose
		_, _, _ = procSetJobInfo.Call(hJob, jobObjectExtendedLimitInfo,
			uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
		_, _, _ = procAssignToJob.Call(hJob, uintptr(pi.Process))
	}
	return &conPTY{inW: inW, outR: outR, hpc: hpc, hPrc: pi.Process, hJob: windows.Handle(hJob)}, nil
}

func (c *conPTY) Read(p []byte) (int, error)  { return c.outR.Read(p) }
func (c *conPTY) Write(p []byte) (int, error) { return c.inW.Write(p) }

func (c *conPTY) Resize(cols, rows uint16) error {
	if cols == 0 || rows == 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.exited {
		return nil // console already torn down by Wait
	}
	size := coord{X: int16(cols), Y: int16(rows)}
	r1, _, err := procResizePseudoConsole.Call(
		uintptr(c.hpc),
		uintptr(*(*uint32)(unsafe.Pointer(&size))),
	)
	if r1 != 0 {
		return err
	}
	return nil
}

func (c *conPTY) Kill() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.exited {
		return nil // Wait already closed the handles
	}
	// Kill the whole job (shell + children); without a job fall back to
	// terminating the shell process itself.
	if c.hJob != 0 {
		_, _, _ = procTerminateJob.Call(uintptr(c.hJob), 1)
	}
	_ = windows.TerminateProcess(c.hPrc, 1)
	return nil
}

func (c *conPTY) Wait() (int, error) {
	ev, err := windows.WaitForSingleObject(c.hPrc, windows.INFINITE)
	if err != nil && ev != windows.WAIT_OBJECT_0 {
		return -1, err
	}
	var code uint32
	_ = windows.GetExitCodeProcess(c.hPrc, &code)
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.exited {
		c.code = int(int32(code))
		c.exited = true
		// Only now is it safe to tear down the pseudoconsole.
		_, _, _ = procClosePseudoConsole.Call(uintptr(c.hpc))
		_ = windows.CloseHandle(c.hPrc)
		c.hPrc = 0
		// Closing the job handle reaps orphaned children via
		// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE.
		if c.hJob != 0 {
			_ = windows.CloseHandle(c.hJob)
			c.hJob = 0
		}
		_ = c.inW.Close()
		_ = c.outR.Close()
	}
	return c.code, nil
}

func (c *conPTY) Close() error {
	_ = c.Kill()
	return nil
}

// ------------------------------------------------------------ pipe fallback ---

type pipePTY struct {
	inW  *os.File
	outR *os.File
	cmd  *exec.Cmd
	once sync.Once
	code int
}

func startPipePTY(shell, cwd string) (PTY, error) {
	cmd := exec.Command(shell, shellArgs(shell)...)
	cmd.Env = os.Environ()
	if cwd != "" {
		cmd.Dir = cwd
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		_ = inR.Close()
		_ = inW.Close()
		return nil, err
	}
	cmd.Stdin = inR
	cmd.Stdout = outW
	cmd.Stderr = outW
	if err := cmd.Start(); err != nil {
		for _, f := range []*os.File{inR, inW, outR, outW} {
			_ = f.Close()
		}
		return nil, err
	}
	// Parent's copies of the child ends.
	_ = inR.Close()
	_ = outW.Close()
	return &pipePTY{inW: inW, outR: outR, cmd: cmd}, nil
}

func (p *pipePTY) Read(p2 []byte) (int, error)  { return p.outR.Read(p2) }
func (p *pipePTY) Write(p2 []byte) (int, error) { return p.inW.Write(p2) }
func (p *pipePTY) Resize(cols, rows uint16) error {
	return nil // no pty on legacy Windows; sizes are ignored
}

func (p *pipePTY) Kill() error {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	return nil
}

func (p *pipePTY) Wait() (int, error) {
	p.once.Do(func() {
		err := p.cmd.Wait()
		p.code = 0
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				p.code = ee.ExitCode()
			} else {
				p.code = -1
			}
		}
		_ = p.inW.Close()
		_ = p.outR.Close()
	})
	return p.code, nil
}

func (p *pipePTY) Close() error {
	_ = p.Kill()
	return nil
}
