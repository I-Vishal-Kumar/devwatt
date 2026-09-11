; devwatt installer. Built by installer\build.ps1 with /DVERSION=x.y.z.
;
; The installer copies the two executables and registers the Apps & Features
; entry; everything that makes devwatt *installed* — the two logon tasks, the
; icon file and the Start menu entry — is done by `devwatt install`, which is
; the single source of truth for that and is what `devwatt uninstall` undoes.
; The installer creates no shortcuts of its own. The finish page starts the
; tray from this elevated installer; the tray hands over to an unelevated
; copy of itself, so that is fine.

Unicode true
!include "MUI2.nsh"
!include "FileFunc.nsh"
!include "LogicLib.nsh"

!ifndef VERSION
  !error "build with /DVERSION=x.y.z"
!endif

Name "devwatt"
OutFile "..\dist\devwatt-setup-${VERSION}.exe"
InstallDir "$PROGRAMFILES64\devwatt"
RequestExecutionLevel admin
SetCompressor /SOLID lzma

VIProductVersion "${VERSION}.0"
VIAddVersionKey "ProductName" "devwatt"
VIAddVersionKey "FileDescription" "devwatt installer"
VIAddVersionKey "FileVersion" "${VERSION}"
VIAddVersionKey "ProductVersion" "${VERSION}"
VIAddVersionKey "LegalCopyright" "Vishal Kumar"

!define UNINST_KEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\devwatt"

!define MUI_ABORTWARNING
!define MUI_FINISHPAGE_RUN "$INSTDIR\devwatt-tray.exe"
!define MUI_FINISHPAGE_RUN_PARAMETERS "--dashboard"
!define MUI_FINISHPAGE_RUN_TEXT "Open devwatt"

!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_LANGUAGE "English"

Section "Install"
  ; The application is 64-bit; without this the HKLM key lands under
  ; WOW6432Node, where Apps & Features lists it as a 32-bit program.
  SetRegView 64

  ; A running tray holds devwatt-tray.exe open, so it is stopped first.
  ; taskkill fails when nothing is running, which is the normal case on a
  ; first install; the result is deliberately ignored.
  nsExec::ExecToLog 'taskkill /F /IM devwatt-tray.exe'
  Pop $0

  SetOutPath "$INSTDIR"
  File "..\bin\devwatt.exe"
  File "..\bin\devwatt-tray.exe"
  WriteUninstaller "$INSTDIR\uninstall.exe"

  WriteRegStr HKLM "${UNINST_KEY}" "DisplayName" "devwatt"
  WriteRegStr HKLM "${UNINST_KEY}" "DisplayVersion" "${VERSION}"
  WriteRegStr HKLM "${UNINST_KEY}" "Publisher" "Vishal Kumar"
  WriteRegStr HKLM "${UNINST_KEY}" "DisplayIcon" "$INSTDIR\devwatt.ico"
  WriteRegStr HKLM "${UNINST_KEY}" "InstallLocation" "$INSTDIR"
  WriteRegStr HKLM "${UNINST_KEY}" "UninstallString" '"$INSTDIR\uninstall.exe"'
  WriteRegStr HKLM "${UNINST_KEY}" "QuietUninstallString" '"$INSTDIR\uninstall.exe" /S'
  WriteRegDWORD HKLM "${UNINST_KEY}" "NoModify" 1
  WriteRegDWORD HKLM "${UNINST_KEY}" "NoRepair" 1
  ${GetSize} "$INSTDIR" "/S=0K" $0 $1 $2
  WriteRegDWORD HKLM "${UNINST_KEY}" "EstimatedSize" $0

  ; The task, the icon and the Start menu entry are what make devwatt
  ; installed; a half-install is worse than none, so a failure here aborts.
  ExecWait '"$INSTDIR\devwatt.exe" install' $0
  ${If} $0 != 0
    MessageBox MB_OK|MB_ICONSTOP "devwatt install failed (exit code $0). The logon task, icon and Start menu entry were not created. Installation aborted."
    Abort
  ${EndIf}
SectionEnd

Section "Uninstall"
  SetRegView 64

  nsExec::ExecToLog 'taskkill /F /IM devwatt-tray.exe'
  Pop $0

  ; Undo the task, the icon and the Start menu entry; the files go regardless.
  ExecWait '"$INSTDIR\devwatt.exe" uninstall' $0
  ${If} $0 != 0
    MessageBox MB_OK|MB_ICONEXCLAMATION "devwatt uninstall reported exit code $0. The files will still be removed."
  ${EndIf}

  Delete "$INSTDIR\devwatt.exe"
  Delete "$INSTDIR\devwatt-tray.exe"
  Delete "$INSTDIR\devwatt.ico"
  Delete "$INSTDIR\uninstall.exe"
  RMDir "$INSTDIR"
  DeleteRegKey HKLM "${UNINST_KEY}"

  ; The WebView2 profile is a cache, not user data. config.json in
  ; %APPDATA%\devwatt is left in place so settings survive a reinstall.
  RMDir /r "$APPDATA\devwatt\webview2"
SectionEnd
