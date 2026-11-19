; The Windows installer, built by scripts\build-windows.ps1 -Installer from
; dist\pg.exe. It installs for the current user only, into
; %LOCALAPPDATA%\Programs\pg, so it needs no admin rights and the app can
; replace its own exe when it updates.

Unicode true
!include "MUI2.nsh"
!include "LogicLib.nsh"

!ifndef VERSION
	!define VERSION "0.0.0"
!endif
!define UNINSTALL_KEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\pg"
; WebView2's id, under which its version is registered once it's installed.
!define WEBVIEW2_KEY "Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}"

Name "pg"
OutFile "..\..\dist\pg-setup.exe"
InstallDir "$LOCALAPPDATA\Programs\pg"
InstallDirRegKey HKCU "${UNINSTALL_KEY}" "InstallLocation"
RequestExecutionLevel user
SetCompressor /SOLID lzma

VIProductVersion "${VERSION}.0"
VIAddVersionKey "ProductName" "pg"
VIAddVersionKey "FileDescription" "pg installer"
VIAddVersionKey "ProductVersion" "${VERSION}"
VIAddVersionKey "FileVersion" "${VERSION}"

!define MUI_ICON "icon.ico"
!define MUI_UNICON "icon.ico"
!define MUI_FINISHPAGE_RUN "$INSTDIR\pg.exe"
!define MUI_FINISHPAGE_RUN_TEXT "Open pg"

!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_LANGUAGE "English"

Function .onInit
	; Windows 10 and 11 come with WebView2, which draws the window, but an
	; older or stripped-down install may not.
	ReadRegStr $0 HKLM "SOFTWARE\WOW6432Node\${WEBVIEW2_KEY}" "pv"
	${If} $0 == ""
		ReadRegStr $0 HKCU "Software\${WEBVIEW2_KEY}" "pv"
	${EndIf}
	${If} $0 == ""
	${OrIf} $0 == "0.0.0.0"
		MessageBox MB_OKCANCEL|MB_ICONEXCLAMATION "pg needs Microsoft Edge WebView2, which isn't installed. Get the Evergreen Runtime from https://developer.microsoft.com/microsoft-edge/webview2/ after this, then open pg." IDOK +2
		Abort
	${EndIf}
FunctionEnd

Section "Install"
	SetOutPath "$INSTDIR"
	; An open pg holds its exe, so ask for it to be closed first.
	SetOverwrite try
	retry:
	ClearErrors
	File "..\..\dist\pg.exe"
	${If} ${Errors}
		MessageBox MB_RETRYCANCEL|MB_ICONEXCLAMATION "pg is open. Quit it, then try again." IDRETRY retry
		Abort
	${EndIf}
	WriteUninstaller "$INSTDIR\uninstall.exe"
	CreateShortcut "$SMPROGRAMS\pg.lnk" "$INSTDIR\pg.exe"

	WriteRegStr HKCU "${UNINSTALL_KEY}" "DisplayName" "pg"
	WriteRegStr HKCU "${UNINSTALL_KEY}" "DisplayIcon" "$INSTDIR\pg.exe"
	WriteRegStr HKCU "${UNINSTALL_KEY}" "DisplayVersion" "${VERSION}"
	WriteRegStr HKCU "${UNINSTALL_KEY}" "Publisher" "pg"
	WriteRegStr HKCU "${UNINSTALL_KEY}" "InstallLocation" "$INSTDIR"
	WriteRegStr HKCU "${UNINSTALL_KEY}" "UninstallString" '"$INSTDIR\uninstall.exe"'
	WriteRegDWORD HKCU "${UNINSTALL_KEY}" "NoModify" 1
	WriteRegDWORD HKCU "${UNINSTALL_KEY}" "NoRepair" 1
SectionEnd

; Leaves the user's own files: saved queries, backups, history and settings.
Section "Uninstall"
	Delete "$SMPROGRAMS\pg.lnk"
	Delete "$INSTDIR\pg.exe"
	Delete "$INSTDIR\uninstall.exe"
	RMDir "$INSTDIR"
	DeleteRegKey HKCU "${UNINSTALL_KEY}"
SectionEnd
