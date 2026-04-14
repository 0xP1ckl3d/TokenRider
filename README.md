# TokenRider

Steal a token. Become the user. Own the shell.

---

## Overview

TokenRider is a Windows post exploitation utility designed to steal and impersonate existing process tokens in order to spawn fully interactive shells under alternate security contexts, including **NT AUTHORITY\SYSTEM** and other logged on users.

Unlike traditional token impersonation tooling, the Go implementation provides a fully interactive PowerShell experience by leveraging ConPTY. This enables proper terminal behaviour including tab completion, PSReadLine support, colours, and responsive input and output, all controlled directly from the originating shell.

The tool operates using a broker and agent model over a named pipe, allowing seamless command execution and interactive session control.

---

## Features

* Token theft and impersonation for SYSTEM and user contexts
* Interactive ConPTY backed PowerShell shells in the Go implementation
* Single command execution mode
* Token enumeration with usability validation
* Safe handling of non spawnable and protected tokens
* Named pipe communication channel between broker and agent
* Automatic privilege handling and elevation prompts

---

## Project Structure

```text
TokenRider/
├── Go/
│   ├── TokenRider.go
│   ├── go.mod
│   └── go.sum
├── PowerShell/
│   ├── TokenRider.ps1
│   └── getsystem.ps1
└── README.md
```

* Go directory contains the Go source code and module files
* PowerShell directory contains both PowerShell implementations

---

## Usage

### Interactive SYSTEM shell

```powershell
TokenRider.exe
```

### Run a single command as SYSTEM

```powershell
TokenRider.exe -c "whoami"
```

### List available user tokens

```powershell
TokenRider.exe -t ?
```

### Impersonate a specific user

```powershell
TokenRider.exe -t DOMAIN\\User
```

---

## How It Works

1. Enumerates running processes and extracts accessible tokens.
2. Filters tokens based on duplication and process spawn capability.
3. Duplicates a usable token into a primary token.
4. Spawns a new process under the impersonated context.
5. Establishes a named pipe between broker and agent.
6. Creates a ConPTY backed shell for interactive sessions.
7. Bridges input and output between the local terminal and the remote shell.

---

## Compilation

### Requirements

* Go 1.20 or newer
* A Windows target

### Build on Windows

```powershell
cd Go
go build -o TokenRider.exe TokenRider.go
```

### Cross compile from Linux

```bash
cd Go
GOOS=windows GOARCH=amd64 go build -o TokenRider.exe TokenRider.go
```

Adjust `GOARCH` as needed for your target architecture.

### Notes

* Must be compiled for Windows due to platform specific APIs
* Cross compilation from Linux is supported as long as the target is Windows
* Requires appropriate privileges such as SeDebugPrivilege at runtime

---

## PowerShell Versions

Two PowerShell implementations are provided.

```text
PowerShell/TokenRider.ps1
PowerShell/getsystem.ps1
```

### TokenRider.ps1

This is the proxy based PowerShell implementation.

* Currently only implemented for SYSTEM token theft
* Does not use ConPTY
* Proxies shell input and output through the existing session
* Provides a less interactive experience than the Go version
* Is a work in progress toward feature parity with the Go implementation

### getsystem.ps1

This is the simpler PowerShell implementation.

* Opens a SYSTEM level command prompt in a new window
* Does not use proxying or pipe based communication
* Intended for cases where same shell execution is not required
* Useful when you have physical access to a system or otherwise do not need in shell interaction
* Provides much more basic functionality than the Go version and the proxy based PowerShell version

---

## Limitations

### Go version

* Terminal size is fixed at launch

  * The ConPTY instance is initialised with the current console dimensions
  * Resizing the window after execution will not update the remote shell size
  * This can result in visual issues such as line wrapping or truncated output

* Requires elevated privileges for token theft

* Cannot impersonate the current user as this causes a deadlock and is intentionally blocked

* Protected process tokens may be visible but are filtered out if not usable

### PowerShell versions

* `TokenRider.ps1` is currently limited to SYSTEM token theft
* `TokenRider.ps1` does not provide ConPTY backed interactivity
* `getsystem.ps1` does not execute within the existing shell
* Feature parity with the Go version is still in progress

---

## Disclaimer

This tool is intended for authorised security testing and research purposes only.
