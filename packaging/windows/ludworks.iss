; Inno Setup script for the Ludworks Windows installer (Inno Setup 6.3 or later).
;
; The installer copies ludworks.exe to {autopf}\Ludworks, registers and starts the
; "Ludworks" Windows service, and can add a firewall rule. Uninstalling stops and removes
; the service; the data directory %ProgramData%\Ludworks is always kept.
;
; The install folder is fixed to {autopf}\Ludworks (C:\Program Files\Ludworks): the
; service runs ludworks.exe from it and the uninstaller runs elevated from it, so it must be
; a folder only administrators can change. A folder such as C:\Ludworks or D:\Ludworks
; inherits "Authenticated Users: Modify" from the drive root, which would let any local
; user replace the binary. There is no folder page, /DIR= and earlier custom folders are
; refused in PrepareToInstall, and so is an existing "Ludworks" service whose program lives
; anywhere else. (Inno's [Dirs] Permissions only add allow entries; they cannot remove
; inherited ones, so they are no substitute.)
;
; Build it after scripts/package.sh (or package.ps1) has staged the Windows files:
;
;   ISCC.exe /DAppVersion=0.1.0 packaging\windows\ludworks.iss
;
; Optional defines:
;   /DFileVersion=0.1.0.0   numeric a.b.c.d version (default: AppVersion, which must then be a.b.c)
;   /DPayloadDir=<dir>      folder holding ludworks.exe, LICENSE, README-windows.txt and
;                           config.example.json (default: ..\..\dist\stage\windows_amd64)
;   /DOutputDir=<dir>       where the setup .exe is written (default: ..\..\dist)

#ifndef AppVersion
  #error Define the version: ISCC /DAppVersion=x.y.z ludworks.iss
#endif
#ifndef FileVersion
  #define FileVersion AppVersion
#endif
#ifndef PayloadDir
  #define PayloadDir "..\..\dist\stage\windows_amd64"
#endif
#ifndef OutputDir
  #define OutputDir "..\..\dist"
#endif

#define ServiceName "Ludworks"
#define Port "8443"
#define FirewallRule "Ludworks (HTTPS 8443)"

[Setup]
AppId={{6F1C3B7E-2A4D-4E8B-9C51-8D3A7F2E4B90}
AppName=Ludworks
AppVersion={#AppVersion}
AppVerName=Ludworks {#AppVersion}
AppPublisher=Ludworks
AppPublisherURL=https://ludworks.readthedocs.io
AppSupportURL=https://ludworks.readthedocs.io
AppCopyright=Copyright (c) 2026 Ludworks. All rights reserved.
VersionInfoVersion={#FileVersion}
VersionInfoDescription=Ludworks setup
DefaultDirName={autopf}\Ludworks
; Always Program Files (see the header): no folder page, and an earlier install in another
; folder is not reused (PrepareToInstall refuses /DIR= and services registered elsewhere).
DisableDirPage=yes
UsePreviousAppDir=no
DisableProgramGroupPage=yes
PrivilegesRequired=admin
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
MinVersion=10.0
OutputDir={#OutputDir}
OutputBaseFilename=ludworks_{#AppVersion}_windows_amd64_setup
SetupIconFile=ludworks.ico
UninstallDisplayIcon={app}\ludworks.exe
UninstallDisplayName=Ludworks
LicenseFile={#PayloadDir}\LICENSE
Compression=lzma2/max
SolidCompression=yes
WizardStyle=modern
; The service is stopped in PrepareToInstall; no Restart Manager prompts.
CloseApplications=no
RestartApplications=no
SetupLogging=yes

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[Messages]
FinishedLabel=Ludworks is installed and running as the Windows service "Ludworks".%n%nOpen https://localhost:{#Port}/ to finish the setup. The one-time setup token is in setup-token.txt in the data folder (normally C:\ProgramData\Ludworks, readable by administrators); the service log ludworks.log in the same folder shows the certificate fingerprint to compare with the browser.

[Tasks]
Name: "firewall"; Description: "Allow other devices on the local network (same subnet) to connect (Windows Firewall, TCP {#Port})"; Flags: unchecked

[Files]
Source: "{#PayloadDir}\ludworks.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#PayloadDir}\README-windows.txt"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#PayloadDir}\LICENSE"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#PayloadDir}\config.example.json"; DestDir: "{app}"; Flags: ignoreversion

[Run]
Filename: "{app}\ludworks.exe"; Parameters: "service install"; StatusMsg: "Registering the Ludworks service..."; Flags: runhidden waituntilterminated; Check: not ServiceExisted
; Upgrade: services registered by older versions kept every default service privilege
; (SeImpersonatePrivilege...). Keep only what the server needs, as "service install" does.
Filename: "{sys}\sc.exe"; Parameters: "privs {#ServiceName} SeChangeNotifyPrivilege"; StatusMsg: "Restricting the service privileges..."; Flags: runhidden waituntilterminated; Check: ServiceExisted
Filename: "{app}\ludworks.exe"; Parameters: "service start";StatusMsg: "Starting the Ludworks service..."; Flags: runhidden waituntilterminated
Filename: "{sys}\netsh.exe"; Parameters: "advfirewall firewall delete rule name=""{#FirewallRule}"""; Tasks: firewall; Flags: runhidden waituntilterminated
; remoteip=localsubnet: only on-link addresses (the LAN's IPv4 subnet and IPv6 prefixes), not
; every address that reaches a private-profile interface (global IPv6, VPNs, port forwards).
Filename: "{sys}\netsh.exe"; Parameters: "advfirewall firewall add rule name=""{#FirewallRule}"" dir=in action=allow protocol=TCP localport={#Port} profile=private,domain remoteip=localsubnet program=""{app}\ludworks.exe"""; StatusMsg: "Adding the firewall rule..."; Tasks: firewall; Flags: runhidden waituntilterminated
; Upgrade without the task: narrow a rule left by an older version, which allowed any remote
; address. netsh fails harmlessly when there is no such rule. Wider access is configured with
; a separate, admin-owned rule (see README-windows.txt), never by editing this one.
Filename: "{sys}\netsh.exe"; Parameters: "advfirewall firewall set rule name=""{#FirewallRule}"" new remoteip=localsubnet"; Tasks: not firewall; Flags: runhidden waituntilterminated
Filename: "{sys}\notepad.exe"; Parameters: """{commonappdata}\Ludworks\setup-token.txt"""; Description: "Show the one-time setup token"; Flags: postinstall nowait skipifsilent; Check: SetupTokenExists
Filename: "https://localhost:{#Port}/"; Description: "Open Ludworks in the browser"; Flags: postinstall shellexec nowait skipifsilent unchecked

[UninstallRun]
Filename: "{app}\ludworks.exe"; Parameters: "service uninstall"; Flags: runhidden waituntilterminated; RunOnceId: "RemoveService"
Filename: "{sys}\netsh.exe"; Parameters: "advfirewall firewall delete rule name=""{#FirewallRule}"""; Flags: runhidden waituntilterminated; RunOnceId: "RemoveFirewallRule"

[Code]
var
  ServiceExistedBefore: Boolean;
  WaitedForToken: Boolean;

function ServiceExists(): Boolean;
var
  ResultCode: Integer;
begin
  { sc.exe exits with 0 when the service exists and 1060 when it does not. }
  Result := Exec(ExpandConstant('{sys}\sc.exe'), 'query {#ServiceName}', '', SW_HIDE,
    ewWaitUntilTerminated, ResultCode) and (ResultCode = 0);
end;

function ServiceExisted(): Boolean;
begin
  Result := ServiceExistedBefore;
end;

{ The program of a service command line such as "C:\dir\ludworks.exe" serve --data X. }
function ImagePathProgram(ImagePath: String): String;
var
  S: String;
  I: Integer;
begin
  S := Trim(ImagePath);
  if Copy(S, 1, 1) = '"' then
  begin
    Delete(S, 1, 1);
    I := Pos('"', S);
  end
  else
    I := Pos(' ', S);
  if I > 0 then
    S := Copy(S, 1, I - 1);
  Result := S;
end;

{ Returns why Ludworks must not be installed here, or an empty string. The service runs
  ludworks.exe as NT SERVICE\Ludworks and the uninstaller runs elevated, both from the
  install folder, so it must be one that only administrators can change: Program Files.
  The comparison is case-insensitive; anything else, including /DIR= on the command line,
  is refused, and so is an existing Ludworks service that runs a program from another
  folder (an earlier install there, or a manual "ludworks.exe service install"). }
function InstallLocationProblem(): String;
var
  AppDir, Wanted, ImagePath, Registered: String;
begin
  Result := '';
  AppDir := ExpandConstant('{app}');
  Wanted := ExpandConstant('{autopf}\Ludworks');
  if CompareText(AddBackslash(AppDir), AddBackslash(Wanted)) <> 0 then
  begin
    Result := 'Ludworks can only be installed in "' + Wanted + '", a folder that only ' +
      'administrators can change: the Ludworks service runs the program from there. ' +
      '"' + AppDir + '" is not allowed.';
    Exit;
  end;
  if RegQueryStringValue(HKLM, 'SYSTEM\CurrentControlSet\Services\{#ServiceName}', 'ImagePath', ImagePath) then
  begin
    Registered := ImagePathProgram(ImagePath);
    if CompareText(Registered, AddBackslash(Wanted) + 'ludworks.exe') <> 0 then
      Result := 'The existing Ludworks service runs "' + Registered + '", which is outside "' +
        Wanted + '". Other users may have been able to change programs in that folder, so do ' +
        'not run anything from it. Remove the old service in an elevated prompt with ' +
        '"sc.exe stop {#ServiceName}" and "sc.exe delete {#ServiceName}", delete that folder, and run ' +
        'this setup again; your data in ' + ExpandConstant('{commonappdata}\Ludworks') +
        ' is kept. Service options such as --data or --listen must then be set again ' +
        '(see the Ludworks documentation, "Managing the service by hand").';
  end;
end;

function PrepareToInstall(var NeedsRestart: Boolean): String;
var
  ResultCode: Integer;
begin
  { Checked before anything is stopped, copied or registered. }
  Result := InstallLocationProblem();
  if Result <> '' then
  begin
    Log('Refusing to install: ' + Result);
    Exit;
  end;
  ServiceExistedBefore := ServiceExists();
  if ServiceExistedBefore then
  begin
    { Upgrade: stop the service so ludworks.exe can be replaced. Stop-Service waits until
      it has stopped; the server drains connections for at most 20 seconds. }
    Exec(ExpandConstant('{sys}\WindowsPowerShell\v1.0\powershell.exe'),
      '-NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "Stop-Service -Name {#ServiceName} -Force -ErrorAction SilentlyContinue"',
      '', SW_HIDE, ewWaitUntilTerminated, ResultCode);
  end;
end;

function SetupTokenExists(): Boolean;
var
  I: Integer;
  TokenFile: String;
begin
  TokenFile := ExpandConstant('{commonappdata}\Ludworks\setup-token.txt');
  Result := FileExists(TokenFile);
  { A freshly started service writes the token within a moment; wait for it once. }
  if (not Result) and (not ServiceExistedBefore) and (not WaitedForToken) then
  begin
    WaitedForToken := True;
    for I := 1 to 20 do
    begin
      Sleep(500);
      Result := FileExists(TokenFile);
      if Result then
        Break;
    end;
  end;
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
begin
  if CurUninstallStep = usPostUninstall then
    SuppressibleMsgBox('Ludworks was removed. Your data was kept in ' +
      ExpandConstant('{commonappdata}\Ludworks') +
      ' (database, files, keys and backups). Delete that folder yourself to remove it for good.',
      mbInformation, MB_OK, IDOK);
end;
