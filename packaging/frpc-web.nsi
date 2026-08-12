Unicode True

!include "MUI2.nsh"
!include "x64.nsh"

!ifndef APP_VERSION
  !define APP_VERSION "1.0.0"
!endif

!define APP_NAME "frpc-web"
!define APP_EXE "frpc-web.exe"
!define APP_ID "9C07B2E7-7D7C-49A6-8EF0-9C3133A57D6D"
!define OUTPUT_DIR "dist"

Name "${APP_NAME}"
OutFile "${OUTPUT_DIR}/frpc-web-for-Windows.exe"
InstallDir "D:\Program Files\frpc-web"
InstallDirRegKey HKLM "Software\${APP_NAME}" "InstallDir"
RequestExecutionLevel admin
SetCompressor /SOLID lzma
Icon "frpc-web.ico"
UninstallIcon "frpc-web.ico"
VIProductVersion "${APP_VERSION}.0"
VIAddVersionKey /LANG=1033 "ProductName" "${APP_NAME}"
VIAddVersionKey /LANG=1033 "ProductVersion" "${APP_VERSION}"
VIAddVersionKey /LANG=1033 "FileVersion" "${APP_VERSION}"
VIAddVersionKey /LANG=1033 "FileDescription" "frpc Windows web console"
VIAddVersionKey /LANG=1033 "LegalCopyright" "frpc-web contributors"

!define MUI_ABORTWARNING
!define MUI_ICON "frpc-web.ico"
!define MUI_UNICON "frpc-web.ico"
!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_LANGUAGE "English"
!insertmacro MUI_LANGUAGE "SimpChinese"

Function .onInit
  ${IfNot} ${RunningX64}
    MessageBox MB_OK|MB_ICONSTOP "${APP_NAME} requires 64-bit Windows."
    Abort
  ${EndIf}
  SetRegView 64
FunctionEnd

Function un.onInit
  SetRegView 64
FunctionEnd

Section "Install"
  SetShellVarContext all
  SetRegView 64
  SetOutPath "$INSTDIR"
  File "${OUTPUT_DIR}/frpc-web.exe"
  File "frpc-web.ico"

  WriteUninstaller "$INSTDIR\uninstall.exe"
  WriteRegStr HKLM "Software\${APP_NAME}" "InstallDir" "$INSTDIR"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${APP_ID}" "DisplayName" "${APP_NAME}"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${APP_ID}" "DisplayVersion" "${APP_VERSION}"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${APP_ID}" "DisplayIcon" "$INSTDIR\${APP_EXE}"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${APP_ID}" "UninstallString" '"$INSTDIR\uninstall.exe"'

  CreateDirectory "$SMPROGRAMS\${APP_NAME}"
  CreateShortcut "$SMPROGRAMS\${APP_NAME}\${APP_NAME}.lnk" "$INSTDIR\${APP_EXE}" 'open --base "$INSTDIR"' "$INSTDIR\frpc-web.ico"
  CreateShortcut "$DESKTOP\${APP_NAME}.lnk" "$INSTDIR\${APP_EXE}" 'open --base "$INSTDIR"' "$INSTDIR\frpc-web.ico"

  ExecWait '"$INSTDIR\${APP_EXE}" install-services-ui --base "$INSTDIR" --no-download'
  IfSilent +2
  Exec '"$INSTDIR\${APP_EXE}" open --base "$INSTDIR"'
SectionEnd

Section "Uninstall"
  SetShellVarContext all
  SetRegView 64
  ExecWait '"$INSTDIR\${APP_EXE}" uninstall --base "$INSTDIR"'

  ; The web console is a resident process and is not a Windows service.
  ; Stop all processes belonging to this installation before removing files.
  ExecWait '"$SYSDIR\taskkill.exe" /IM ${APP_EXE} /F /T'
  ExecWait '"$SYSDIR\taskkill.exe" /IM frpc.exe /F /T'

  Delete "$DESKTOP\${APP_NAME}.lnk"
  Delete "$SMPROGRAMS\${APP_NAME}\${APP_NAME}.lnk"
  ; Remove shortcuts created by both the current and older installers.
  Delete "$SMPROGRAMS\${APP_NAME}.lnk"
  RMDir /r "$SMPROGRAMS\${APP_NAME}"
  ; Remove configuration, logs, downloaded frpc and all other install data.
  RMDir /r "$INSTDIR"

  DeleteRegKey HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${APP_ID}"
  DeleteRegKey HKLM "Software\${APP_NAME}"
SectionEnd
