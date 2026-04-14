param(
    [string]$c
)

Set-StrictMode -Version Latest

Add-Type -TypeDefinition @"
using System;
using System.Runtime.InteropServices;

public class Win32TokenLauncher
{
    [StructLayout(LayoutKind.Sequential)]
    public struct LUID {
        public uint LowPart;
        public int HighPart;
    }

    [StructLayout(LayoutKind.Sequential)]
    public struct LUID_AND_ATTRIBUTES {
        public LUID Luid;
        public uint Attributes;
    }

    [StructLayout(LayoutKind.Sequential)]
    public struct TOKEN_PRIVILEGES {
        public uint PrivilegeCount;
        public LUID_AND_ATTRIBUTES Privileges;
    }

    [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)]
    public struct STARTUPINFO {
        public uint cb;
        public string lpReserved;
        public string lpDesktop;
        public string lpTitle;
        public uint dwX;
        public uint dwY;
        public uint dwXSize;
        public uint dwYSize;
        public uint dwXCountChars;
        public uint dwYCountChars;
        public uint dwFillAttribute;
        public uint dwFlags;
        public short wShowWindow;
        public short cbReserved2;
        public IntPtr lpReserved2;
        public IntPtr hStdInput;
        public IntPtr hStdOutput;
        public IntPtr hStdError;
    }

    [StructLayout(LayoutKind.Sequential)]
    public struct PROCESS_INFORMATION {
        public IntPtr hProcess;
        public IntPtr hThread;
        public uint dwProcessId;
        public uint dwThreadId;
    }

    public const uint PROCESS_QUERY_LIMITED_INFORMATION = 0x1000;
    public const uint TOKEN_ASSIGN_PRIMARY    = 0x0001;
    public const uint TOKEN_DUPLICATE         = 0x0002;
    public const uint TOKEN_QUERY             = 0x0008;
    public const uint TOKEN_ADJUST_PRIVILEGES = 0x0020;
    public const uint TOKEN_ALL_ACCESS        = 0xF01FF;
    public const uint SE_PRIVILEGE_ENABLED    = 0x00000002;
    public const int  SecurityImpersonation   = 2;
    public const int  TokenPrimary            = 1;
    public const uint CREATE_NO_WINDOW        = 0x08000000;

    [DllImport("kernel32.dll", SetLastError=true)]
    public static extern IntPtr OpenProcess(uint dwDesiredAccess, bool bInheritHandle, uint dwProcessId);

    [DllImport("advapi32.dll", SetLastError=true)]
    public static extern bool OpenProcessToken(IntPtr ProcessHandle, uint DesiredAccess, out IntPtr TokenHandle);

    [DllImport("advapi32.dll", SetLastError=true, CharSet=CharSet.Unicode)]
    public static extern bool LookupPrivilegeValue(string lpSystemName, string lpName, out LUID lpLuid);

    [DllImport("advapi32.dll", SetLastError=true)]
    public static extern bool AdjustTokenPrivileges(
        IntPtr TokenHandle, bool DisableAllPrivileges,
        ref TOKEN_PRIVILEGES NewState, uint BufferLength,
        IntPtr PreviousState, IntPtr ReturnLength);

    [DllImport("advapi32.dll", SetLastError=true)]
    public static extern bool DuplicateTokenEx(
        IntPtr hExistingToken, uint dwDesiredAccess,
        IntPtr lpTokenAttributes, int ImpersonationLevel,
        int TokenType, out IntPtr phNewToken);

    [DllImport("advapi32.dll", SetLastError=true, CharSet=CharSet.Unicode)]
    public static extern bool CreateProcessWithTokenW(
        IntPtr hToken, uint dwLogonFlags,
        string lpApplicationName, string lpCommandLine,
        uint dwCreationFlags, IntPtr lpEnvironment,
        string lpCurrentDirectory,
        ref STARTUPINFO lpStartupInfo,
        out PROCESS_INFORMATION lpProcessInformation);

    [DllImport("kernel32.dll", SetLastError=true)]
    public static extern bool CloseHandle(IntPtr hObject);
}
"@

# ── module-level state ────────────────────────────────────────────────────────

$script:SystemProxyState     = $null
$script:PipeConnectTimeoutMs = 15000

# Executables that are inherently interactive (REPLs, console tools, etc.).
# Commands whose first token matches an entry here are automatically promoted
# to a new-window launch instead of running inline through the pipe.
$script:KnownInteractiveProcesses = [System.Collections.Generic.HashSet[string]]::new(
    [System.StringComparer]::OrdinalIgnoreCase
)
@(
    'bash',       'bash.exe',
    'cmd',        'cmd.exe',
    'debug',      'debug.exe',
    'diskpart',   'diskpart.exe',
    'ftp',        'ftp.exe',
    'mmc',        'mmc.exe',
    'mongo',      'mongosh',
    'mysql',      'mysql.exe',
    'netsh',      'netsh.exe',
    'node',       'node.exe',
    'nslookup',
    'powershell', 'powershell.exe',
    'psql',       'psql.exe',
    'pwsh',       'pwsh.exe',
    'python',     'python.exe',
    'python3',    'python3.exe',
    'redis-cli',
    'regedit',    'regedit.exe',
    'runas',      'runas.exe',
    'sh',         'sh.exe',
    'sqlcmd',     'sqlcmd.exe',
    'ssh',        'ssh.exe',
    'telnet',     'telnet.exe',
    'wsl',        'wsl.exe'
) | ForEach-Object { [void]$script:KnownInteractiveProcesses.Add($_) }

# ── agent script factory ──────────────────────────────────────────────────────

function New-SystemProxyAgentScript {
    param(
        [Parameter(Mandatory)][string]$Path
    )

    $agentCode = @'
param(
    [Parameter(Mandatory)]
    [string]$PipeName
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

$pipe = [System.IO.Pipes.NamedPipeClientStream]::new(
    ".",
    $PipeName,
    [System.IO.Pipes.PipeDirection]::InOut,
    [System.IO.Pipes.PipeOptions]::None
)
$pipe.Connect(15000)

$enc    = [System.Text.UTF8Encoding]::new($false)
$reader = [System.IO.StreamReader]::new($pipe, $enc)
$writer = [System.IO.StreamWriter]::new($pipe, $enc)
$writer.AutoFlush = $true

try {
    while ($true) {
        $line = $reader.ReadLine()
        if ($null -eq $line) { break }

        $msg     = $line | ConvertFrom-Json
        $id      = [string]$msg.id
        $type    = [string]$msg.type
        $payload = [string]$msg.payload

        if ($type -eq "exit") {
            $writer.WriteLine((@{ id = $id; ok = $true; output = "" } | ConvertTo-Json -Compress))
            break
        }

        if ($type -eq "cd") {
            try {
                Set-Location -Path $payload
                $out = (Get-Location).Path
                $writer.WriteLine((@{ id = $id; ok = $true; output = $out } | ConvertTo-Json -Compress))
            }
            catch {
                $writer.WriteLine((@{ id = $id; ok = $false; output = ($_ | Out-String) } | ConvertTo-Json -Compress))
            }
            continue
        }

        if ($type -eq "start") {
            try {
                Start-Process -FilePath "cmd.exe" -ArgumentList "/c $payload" -WindowStyle Normal | Out-Null
                $writer.WriteLine((@{ id = $id; ok = $true; output = "[started in new window]" } | ConvertTo-Json -Compress))
            }
            catch {
                $writer.WriteLine((@{ id = $id; ok = $false; output = ($_ | Out-String) } | ConvertTo-Json -Compress))
            }
            continue
        }

        # Default: exec
        try {
            $result = & { Invoke-Expression $payload 2>&1 | Out-String }
            if ($null -eq $result) { $result = "" }
            $writer.WriteLine((@{ id = $id; ok = $true; output = $result } | ConvertTo-Json -Compress))
        }
        catch {
            $writer.WriteLine((@{ id = $id; ok = $false; output = ($_ | Out-String) } | ConvertTo-Json -Compress))
        }
    }
}
finally {
    $writer.Dispose()
    $reader.Dispose()
    $pipe.Dispose()
}
'@

    $agentCode | Set-Content -Path $Path -Encoding UTF8
}

# ── public functions ──────────────────────────────────────────────────────────

function Start-SystemProxy {
    if ($script:SystemProxyState) {
        throw "System proxy already running. Call Stop-SystemProxy first."
    }

    $pipeName  = "SysProxy_{0}"  -f ([guid]::NewGuid().ToString("N"))
    $agentPath = Join-Path $env:TEMP ("sysproxy_agent_{0}.ps1" -f ([guid]::NewGuid().ToString("N")))

    New-SystemProxyAgentScript -Path $agentPath

    $server = [System.IO.Pipes.NamedPipeServerStream]::new(
        $pipeName,
        [System.IO.Pipes.PipeDirection]::InOut,
        1,
        [System.IO.Pipes.PipeTransmissionMode]::Byte,
        [System.IO.Pipes.PipeOptions]::None
    )

    $currentSession = [System.Diagnostics.Process]::GetCurrentProcess().SessionId
    $winlogon = Get-Process winlogon -ErrorAction SilentlyContinue |
                Where-Object { $_.SessionId -eq $currentSession } |
                Select-Object -First 1

    if (-not $winlogon) {
        $server.Dispose()
        Remove-Item $agentPath -Force -ErrorAction SilentlyContinue
        throw "No winlogon.exe found in session $currentSession. Are you running elevated?"
    }

    $hProc = [Win32TokenLauncher]::OpenProcess(
        [Win32TokenLauncher]::PROCESS_QUERY_LIMITED_INFORMATION,
        $false,
        [uint32]$winlogon.Id
    )
    if ($hProc -eq [IntPtr]::Zero) {
        $server.Dispose()
        Remove-Item $agentPath -Force -ErrorAction SilentlyContinue
        throw "OpenProcess(winlogon) failed (error $([Runtime.InteropServices.Marshal]::GetLastWin32Error())). Run as Administrator."
    }

    try {
        $hSelfTok = [IntPtr]::Zero
        if (-not [Win32TokenLauncher]::OpenProcessToken(
            [System.Diagnostics.Process]::GetCurrentProcess().Handle,
            [Win32TokenLauncher]::TOKEN_ADJUST_PRIVILEGES -bor [Win32TokenLauncher]::TOKEN_QUERY,
            [ref]$hSelfTok
        )) { throw "OpenProcessToken(self) failed: $([Runtime.InteropServices.Marshal]::GetLastWin32Error())" }

        try {
            $luid = New-Object Win32TokenLauncher+LUID
            if (-not [Win32TokenLauncher]::LookupPrivilegeValue($null, "SeDebugPrivilege", [ref]$luid)) {
                throw "LookupPrivilegeValue failed: $([Runtime.InteropServices.Marshal]::GetLastWin32Error())"
            }

            $tp = New-Object Win32TokenLauncher+TOKEN_PRIVILEGES
            $tp.PrivilegeCount        = 1
            $tp.Privileges            = New-Object Win32TokenLauncher+LUID_AND_ATTRIBUTES
            $tp.Privileges.Luid       = $luid
            $tp.Privileges.Attributes = [Win32TokenLauncher]::SE_PRIVILEGE_ENABLED

            # AdjustTokenPrivileges returns FALSE only on hard failure.
            # ERROR_NOT_ALL_ASSIGNED (1300) with TRUE return is normal when the
            # privilege is already at the desired state — do not treat as error.
            $adjOk = [Win32TokenLauncher]::AdjustTokenPrivileges(
                $hSelfTok, $false, [ref]$tp, 0, [IntPtr]::Zero, [IntPtr]::Zero
            )
            if (-not $adjOk) {
                throw "AdjustTokenPrivileges returned FALSE: $([Runtime.InteropServices.Marshal]::GetLastWin32Error())"
            }

            $hSysTok = [IntPtr]::Zero
            if (-not [Win32TokenLauncher]::OpenProcessToken(
                $hProc,
                [Win32TokenLauncher]::TOKEN_DUPLICATE -bor
                [Win32TokenLauncher]::TOKEN_ASSIGN_PRIMARY -bor
                [Win32TokenLauncher]::TOKEN_QUERY,
                [ref]$hSysTok
            )) { throw "OpenProcessToken(winlogon) failed: $([Runtime.InteropServices.Marshal]::GetLastWin32Error())" }

            try {
                $hPrimary = [IntPtr]::Zero
                if (-not [Win32TokenLauncher]::DuplicateTokenEx(
                    $hSysTok,
                    [Win32TokenLauncher]::TOKEN_ALL_ACCESS,
                    [IntPtr]::Zero,
                    [Win32TokenLauncher]::SecurityImpersonation,
                    [Win32TokenLauncher]::TokenPrimary,
                    [ref]$hPrimary
                )) { throw "DuplicateTokenEx failed: $([Runtime.InteropServices.Marshal]::GetLastWin32Error())" }

                try {
                    $psi    = New-Object Win32TokenLauncher+STARTUPINFO
                    $psi.cb = [Runtime.InteropServices.Marshal]::SizeOf([type][Win32TokenLauncher+STARTUPINFO])
                    $pi     = New-Object Win32TokenLauncher+PROCESS_INFORMATION

                    $psExe   = "$env:WINDIR\System32\WindowsPowerShell\v1.0\powershell.exe"
                    $cmdLine = "`"$psExe`" -NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -File `"$agentPath`" -PipeName `"$pipeName`""

                    $ok = [Win32TokenLauncher]::CreateProcessWithTokenW(
                        $hPrimary, 0, $psExe, $cmdLine,
                        [Win32TokenLauncher]::CREATE_NO_WINDOW,
                        [IntPtr]::Zero, $env:WINDIR,
                        [ref]$psi, [ref]$pi
                    )
                    if (-not $ok) {
                        throw "CreateProcessWithTokenW failed: $([Runtime.InteropServices.Marshal]::GetLastWin32Error())"
                    }

                    [Win32TokenLauncher]::CloseHandle($pi.hThread)  | Out-Null
                    [Win32TokenLauncher]::CloseHandle($pi.hProcess) | Out-Null
                }
                finally { [Win32TokenLauncher]::CloseHandle($hPrimary) | Out-Null }
            }
            finally { [Win32TokenLauncher]::CloseHandle($hSysTok) | Out-Null }
        }
        finally { [Win32TokenLauncher]::CloseHandle($hSelfTok) | Out-Null }
    }
    finally { [Win32TokenLauncher]::CloseHandle($hProc) | Out-Null }

    # Wait for agent to connect — timeout prevents hanging when the spawned
    # process dies before reaching pipe.Connect().
    $connectTask = $server.WaitForConnectionAsync()
    if (-not $connectTask.Wait($script:PipeConnectTimeoutMs)) {
        $server.Dispose()
        Remove-Item $agentPath -Force -ErrorAction SilentlyContinue
        throw ("Agent did not connect within {0} ms. " +
               "Verify SeDebugPrivilege, Administrator rights, and execution policy.") -f $script:PipeConnectTimeoutMs
    }

    $enc    = [System.Text.UTF8Encoding]::new($false)
    $reader = [System.IO.StreamReader]::new($server, $enc)
    $writer = [System.IO.StreamWriter]::new($server, $enc)
    $writer.AutoFlush = $true

    $script:SystemProxyState = [pscustomobject]@{
        PipeName   = $pipeName
        AgentPath  = $agentPath
        Server     = $server
        Reader     = $reader
        Writer     = $writer
        CurrentDir = (Get-Location).Path
        Seq        = 0
        DrainNext  = $false
    }

    "SYSTEM proxy started."
}

function Invoke-SystemProxy {
    param(
        [Parameter(Mandatory)][string]$Command
    )

    if (-not $script:SystemProxyState) {
        throw "SYSTEM proxy is not running. Call Start-SystemProxy first."
    }

    $trimmed = $Command.Trim()

    # cd / Set-Location
    if ($trimmed -match '^(cd|Set-Location)\s+(.+)$') {
        $target  = $Matches[2].Trim().Trim('"')
        $id      = [guid]::NewGuid().ToString("N")
        $payload = @{ id = $id; type = "cd"; payload = $target } | ConvertTo-Json -Compress

        $script:SystemProxyState.Writer.WriteLine($payload)
        $resp = $script:SystemProxyState.Reader.ReadLine() | ConvertFrom-Json
        if (-not $resp.ok) { throw [string]$resp.output }
        $script:SystemProxyState.CurrentDir = [string]$resp.output
        return
    }

    # Explicit new-window launch via 'start'
    if ($trimmed -match '^start\s+(.+)$') {
        $id      = [guid]::NewGuid().ToString("N")
        $payload = @{ id = $id; type = "start"; payload = $Matches[1].Trim() } | ConvertTo-Json -Compress

        $script:SystemProxyState.Writer.WriteLine($payload)
        $resp = $script:SystemProxyState.Reader.ReadLine() | ConvertFrom-Json
        if (-not $resp.ok) { throw [string]$resp.output }
        Write-Host $resp.output
        return
    }

    # Interactive process detection
    $firstToken = ($trimmed -split '\s+')[0]
    $exeName    = [System.IO.Path]::GetFileNameWithoutExtension($firstToken)
    $exeWithExt = [System.IO.Path]::GetFileName($firstToken)

    if ($script:KnownInteractiveProcesses.Contains($exeName) -or
        $script:KnownInteractiveProcesses.Contains($exeWithExt)) {

        $id      = [guid]::NewGuid().ToString("N")
        $payload = @{ id = $id; type = "start"; payload = $trimmed } | ConvertTo-Json -Compress

        $script:SystemProxyState.Writer.WriteLine($payload)
        $resp = $script:SystemProxyState.Reader.ReadLine() | ConvertFrom-Json
        if (-not $resp.ok) { throw [string]$resp.output }
        return
    }

    # Normal exec
    $id      = [guid]::NewGuid().ToString("N")
    $payload = @{ id = $id; type = "exec"; payload = $trimmed } | ConvertTo-Json -Compress

    if ($script:SystemProxyState.DrainNext) {
        $script:SystemProxyState.Reader.ReadLine() | Out-Null
        $script:SystemProxyState.DrainNext = $false
    }

    $script:SystemProxyState.Writer.WriteLine($payload)

    # FIX: remove async entirely (this is what is breaking under StrictMode)
    $respLine = $script:SystemProxyState.Reader.ReadLine()

    if ([string]::IsNullOrWhiteSpace($respLine)) {
        throw "No response received from SYSTEM agent."
    }

    $resp = $respLine | ConvertFrom-Json
    if (-not $resp.ok) { throw [string]$resp.output }

    $out = [string]$resp.output
    if ($out.Length -gt 0) { $out.TrimEnd("`r", "`n") }
}

function Stop-SystemProxy {
    if (-not $script:SystemProxyState) { return }

    try {
        $payload = @{ id = [guid]::NewGuid().ToString("N"); type = "exit"; payload = "" } | ConvertTo-Json -Compress
        $script:SystemProxyState.Writer.WriteLine($payload) | Out-Null
        [void]$script:SystemProxyState.Reader.ReadLine()
    }
    catch {}

    try { $script:SystemProxyState.Writer.Dispose() } catch {}
    try { $script:SystemProxyState.Reader.Dispose() } catch {}
    try { $script:SystemProxyState.Server.Dispose()  } catch {}
    try { Remove-Item $script:SystemProxyState.AgentPath -Force -ErrorAction SilentlyContinue } catch {}

    $script:SystemProxyState = $null
    "SYSTEM proxy stopped."
}

function Start-SystemProxyShell {
    if (-not $script:SystemProxyState) {
        Start-SystemProxy | Out-Null
    }

    # Welcome banner
    Write-Host "  SYSTEM Proxy Shell [NT AUTHORITY\SYSTEM]" -ForegroundColor Cyan
    Write-Host "  Interactive processes launch in a new window automatically." -ForegroundColor DarkGray
    Write-Host "  Use 'start <cmd>' to force a new window. Type 'exit' to quit." -ForegroundColor DarkGray
    Write-Host ""

    # Shell loop — labelled so that break/continue inside the switch target the
    # loop, not just the switch block (classic PowerShell gotcha).
    :shell while ($true) {
        $prompt = "[SYSTEM] PS $($script:SystemProxyState.CurrentDir)> "

        try {
            $cmd = Read-Host -Prompt $prompt
        }
        catch {
            break shell   # Ctrl-C / EOF
        }

        if ($null -eq $cmd) { continue shell }

        switch -Regex ($cmd.Trim()) {
            '^(exit|quit)$' { break shell   }
            '^$'            { continue shell }
            default {
                try {
                    $result = Invoke-SystemProxy -Command $cmd
                    if ($null -ne $result -and $result -ne '') {
                        Write-Host $result
                    }
                }
                catch {
                    Write-Host "ERROR: $($_.Exception.Message)" -ForegroundColor Red
                }
            }
        }
    }

    Stop-SystemProxy | Out-Null
}

if ($PSCommandPath -like '*.psm1') {
    Export-ModuleMember -Function Start-SystemProxy, Invoke-SystemProxy, Stop-SystemProxy, Start-SystemProxyShell
} else {
    if ($c) {
        Start-SystemProxy | Out-Null
        try   { Invoke-SystemProxy -Command $c }
        finally { Stop-SystemProxy | Out-Null }
    } else {
        Start-SystemProxyShell
    }
}
