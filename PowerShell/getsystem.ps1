Add-Type -TypeDefinition @"
using System;
using System.Runtime.InteropServices;

public class Win32 {
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
    public const uint TOKEN_ASSIGN_PRIMARY   = 0x0001;
    public const uint TOKEN_DUPLICATE        = 0x0002;
    public const uint TOKEN_QUERY            = 0x0008;
    public const uint TOKEN_ADJUST_PRIVILEGES= 0x0020;
    public const uint TOKEN_ALL_ACCESS       = 0xF01FF;
    public const uint SE_PRIVILEGE_ENABLED   = 0x00000002;
    public const int  SecurityImpersonation  = 2;
    public const int  TokenPrimary           = 1;
    public const uint CREATE_NEW_CONSOLE     = 0x00000010;

    [DllImport("kernel32.dll", SetLastError=true)]
    public static extern IntPtr OpenProcess(uint dwDesiredAccess, bool bInheritHandle, uint dwProcessId);

    [DllImport("advapi32.dll", SetLastError=true)]
    public static extern bool OpenProcessToken(IntPtr ProcessHandle, uint DesiredAccess, out IntPtr TokenHandle);

    [DllImport("advapi32.dll", SetLastError=true, CharSet=CharSet.Unicode)]
    public static extern bool LookupPrivilegeValue(string lpSystemName, string lpName, out LUID lpLuid);

    [DllImport("advapi32.dll", SetLastError=true)]
    public static extern bool AdjustTokenPrivileges(IntPtr TokenHandle, bool DisableAllPrivileges, ref TOKEN_PRIVILEGES NewState, uint BufferLength, IntPtr PreviousState, IntPtr ReturnLength);

    [DllImport("advapi32.dll", SetLastError=true)]
    public static extern bool DuplicateTokenEx(IntPtr hExistingToken, uint dwDesiredAccess, IntPtr lpTokenAttributes, int ImpersonationLevel, int TokenType, out IntPtr phNewToken);

    [DllImport("advapi32.dll", SetLastError=true, CharSet=CharSet.Unicode)]
    public static extern bool CreateProcessWithTokenW(
        IntPtr hToken,
        uint dwLogonFlags,
        string lpApplicationName,
        string lpCommandLine,
        uint dwCreationFlags,
        IntPtr lpEnvironment,
        string lpCurrentDirectory,
        ref STARTUPINFO lpStartupInfo,
        out PROCESS_INFORMATION lpProcessInformation
    );

    [DllImport("kernel32.dll", SetLastError=true)]
    public static extern bool CloseHandle(IntPtr hObject);
}
"@

$proc = Get-Process winlogon | Select-Object -First 1
if (-not $proc) { throw "No winlogon.exe found" }

$hProc = [Win32]::OpenProcess([Win32]::PROCESS_QUERY_LIMITED_INFORMATION, $false, [uint32]$proc.Id)
if ($hProc -eq [IntPtr]::Zero) { throw "OpenProcess failed: $([Runtime.InteropServices.Marshal]::GetLastWin32Error())" }

$hSelfTok = [IntPtr]::Zero
if (-not [Win32]::OpenProcessToken([System.Diagnostics.Process]::GetCurrentProcess().Handle, [Win32]::TOKEN_ADJUST_PRIVILEGES -bor [Win32]::TOKEN_QUERY, [ref]$hSelfTok)) {
    throw "OpenProcessToken(self) failed: $([Runtime.InteropServices.Marshal]::GetLastWin32Error())"
}

$luid = New-Object Win32+LUID
if (-not [Win32]::LookupPrivilegeValue($null, "SeDebugPrivilege", [ref]$luid)) {
    throw "LookupPrivilegeValue failed: $([Runtime.InteropServices.Marshal]::GetLastWin32Error())"
}

$tp = New-Object Win32+TOKEN_PRIVILEGES
$tp.PrivilegeCount = 1
$tp.Privileges = New-Object Win32+LUID_AND_ATTRIBUTES
$tp.Privileges.Luid = $luid
$tp.Privileges.Attributes = [Win32]::SE_PRIVILEGE_ENABLED
[void][Win32]::AdjustTokenPrivileges($hSelfTok, $false, [ref]$tp, 0, [IntPtr]::Zero, [IntPtr]::Zero)

$hSysTok = [IntPtr]::Zero
if (-not [Win32]::OpenProcessToken($hProc, [Win32]::TOKEN_DUPLICATE -bor [Win32]::TOKEN_ASSIGN_PRIMARY -bor [Win32]::TOKEN_QUERY, [ref]$hSysTok)) {
    throw "OpenProcessToken(winlogon) failed: $([Runtime.InteropServices.Marshal]::GetLastWin32Error())"
}

$hPrimary = [IntPtr]::Zero
if (-not [Win32]::DuplicateTokenEx($hSysTok, [Win32]::TOKEN_ALL_ACCESS, [IntPtr]::Zero, [Win32]::SecurityImpersonation, [Win32]::TokenPrimary, [ref]$hPrimary)) {
    throw "DuplicateTokenEx failed: $([Runtime.InteropServices.Marshal]::GetLastWin32Error())"
}

$si = New-Object Win32+STARTUPINFO
$si.cb = [Runtime.InteropServices.Marshal]::SizeOf([type][Win32+STARTUPINFO])
$pi = New-Object Win32+PROCESS_INFORMATION

if (-not [Win32]::CreateProcessWithTokenW($hPrimary, 0, "$env:WINDIR\System32\cmd.exe", $null, [Win32]::CREATE_NEW_CONSOLE, [IntPtr]::Zero, (Get-Location).Path, [ref]$si, [ref]$pi)) {
    throw "CreateProcessWithTokenW failed: $([Runtime.InteropServices.Marshal]::GetLastWin32Error())"
}

[Win32]::CloseHandle($pi.hThread) | Out-Null
[Win32]::CloseHandle($pi.hProcess) | Out-Null
[Win32]::CloseHandle($hPrimary) | Out-Null
[Win32]::CloseHandle($hSysTok) | Out-Null
[Win32]::CloseHandle($hSelfTok) | Out-Null
[Win32]::CloseHandle($hProc) | Out-Null