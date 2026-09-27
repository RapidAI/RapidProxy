Unicode true

####
## RapidProxy 双架构安装包：内含 amd64 + 386 两个程序，安装时自动检测
## 系统（处理器）架构选择对应版本。手动编译示例：
## > makensis -DARG_WAILS_AMD64_BINARY=..\..\bin\RapidProxy-amd64.exe ^
##            -DARG_RAPIDPROXY_386_BINARY=..\..\bin\RapidProxy-386.exe project.nsi
####
## The following information is taken from the ProjectInfo file, but they can be overwritten here.
####
## !define INFO_PROJECTNAME    "MyProject" # Default "{{.Name}}"
## !define INFO_COMPANYNAME    "MyCompany" # Default "{{.Info.CompanyName}}"
## !define INFO_PRODUCTNAME    "MyProduct" # Default "{{.Info.ProductName}}"
## !define INFO_PRODUCTVERSION "1.0.0"     # Default "{{.Info.ProductVersion}}"
## !define INFO_COPYRIGHT      "Copyright" # Default "{{.Info.Copyright}}"
###
## !define PRODUCT_EXECUTABLE  "Application.exe"      # Default "${INFO_PROJECTNAME}.exe"
## !define UNINST_KEY_NAME     "UninstKeyInRegistry"  # Default "${INFO_COMPANYNAME}${INFO_PRODUCTNAME}"
####
## !define REQUEST_EXECUTION_LEVEL "admin"            # Default "admin"  see also https://nsis.sourceforge.io/Docs/Chapter4.html
####
## 清单用 asInvoker（user）：允许普通权限进程直接拉起安装包——旧版本的
## 在线更新就是 fork/exec 本安装包，requireAdministrator 清单会被 Windows
## 直接拒绝（requires elevation）。真正的提权在 .onInit 里做（自提权）。
####
!define REQUEST_EXECUTION_LEVEL "user"
## Include the wails tools
####
!include "wails_tools.nsh"

# 32 位（386）程序路径：未提供时安装包仅支持 64 位系统
!ifdef ARG_RAPIDPROXY_386_BINARY
    !define SUPPORTS_X86
!endif

# The version information for this two must consist of 4 parts
VIProductVersion "${INFO_PRODUCTVERSION}.0"
VIFileVersion    "${INFO_PRODUCTVERSION}.0"

VIAddVersionKey "CompanyName"     "${INFO_COMPANYNAME}"
VIAddVersionKey "FileDescription" "${INFO_PRODUCTNAME} Installer"
VIAddVersionKey "ProductVersion"  "${INFO_PRODUCTVERSION}"
VIAddVersionKey "FileVersion"     "${INFO_PRODUCTVERSION}"
VIAddVersionKey "LegalCopyright"  "${INFO_COPYRIGHT}"
VIAddVersionKey "ProductName"     "${INFO_PRODUCTNAME}"

# Enable HiDPI support. https://nsis.sourceforge.io/Reference/ManifestDPIAware
ManifestDPIAware true

!include "MUI.nsh"

!define MUI_ICON "..\icon.ico"
!define MUI_UNICON "..\icon.ico"
# !define MUI_WELCOMEFINISHPAGE_BITMAP "resources\leftimage.bmp" #Include this to add a bitmap on the left side of the Welcome Page. Must be a size of 164x314
!define MUI_FINISHPAGE_NOAUTOCLOSE # Wait on the INSTFILES page so the user can take a look into the details of the installation steps
!define MUI_ABORTWARNING # This will warn the user if they exit from the installer.

# 安装完成后自动运行（完成页默认勾选的复选框）
!define MUI_FINISHPAGE_RUN "$INSTDIR\${PRODUCT_EXECUTABLE}"
!define MUI_FINISHPAGE_RUN_TEXT "安装完成后运行 ${INFO_PRODUCTNAME}"

!insertmacro MUI_PAGE_WELCOME # Welcome to the installer page.
# !insertmacro MUI_PAGE_LICENSE "resources\eula.txt" # Adds a EULA page to the installer
!insertmacro MUI_PAGE_DIRECTORY # In which folder install page.
!insertmacro MUI_PAGE_INSTFILES # Installing page.
!insertmacro MUI_PAGE_FINISH # Finished installation page.

!insertmacro MUI_UNPAGE_INSTFILES # Uinstalling page

!insertmacro MUI_LANGUAGE "English" # Set the Language of the installer

## The following two statements can be used to sign the installer and the uninstaller. The path to the binaries are provided in %1
#!uninstfinalize 'signtool --file "%1"'
#!finalize 'signtool --file "%1"'

Name "${INFO_PRODUCTNAME}"
OutFile "..\..\bin\${INFO_PROJECTNAME}-windows-setup.exe" # 双架构合一安装包
InstallDir "$PROGRAMFILES64\${INFO_COMPANYNAME}\${INFO_PRODUCTNAME}" # .onInit 按架构重设
ShowInstDetails show # This will always show the installation details.

Function .onInit
    # 自提权：非提权会话（管理员普通运行、标准用户、旧版本在线更新直接
    # exec 本安装包）在写 Program Files 前必须提升为管理员。用 runas 重新
    # 拉起自己触发 UAC；用户取消授权则直接退出，不进安装向导。
    UserInfo::GetAccountType
    Pop $0
    ${If} $0 != "admin"
        ExecShell "runas" "$EXEPATH"
        Quit
    ${EndIf}

    # 注意：不使用 wails.checkArchitecture（它不支持 32 位系统）。
    ${If} ${AtLeastWin10}
        ; x86 / x64 均受支持
    ${Else}
        MessageBox MB_OK "${INFO_PRODUCTNAME} 需要 Windows 10（Server 2016）或更高版本。"
        Quit
    ${EndIf}

    # 按系统架构选择安装目录（32 位系统上 $PROGRAMFILES32 即 $PROGRAMFILES）
    ${If} ${RunningX64}
        StrCpy $INSTDIR "$PROGRAMFILES64\${INFO_COMPANYNAME}\${INFO_PRODUCTNAME}"
    ${Else}
        StrCpy $INSTDIR "$PROGRAMFILES32\${INFO_COMPANYNAME}\${INFO_PRODUCTNAME}"
    ${EndIf}
FunctionEnd

Section
    # 本安装包始终以管理员身份运行（见 .onInit 自提权），且历史版本均按
    # 全机（all）上下文安装，这里固定 all，保持与既有安装一致；
    # 不用 wails.setShellContext（它会按 REQUEST_EXECUTION_LEVEL=user 走 current）。
    SetShellVarContext all

    !insertmacro wails.webview2runtime

    SetOutPath $INSTDIR

    # 按运行时架构安装对应版本的程序
    ${If} ${RunningX64}
        File "/oname=${PRODUCT_EXECUTABLE}" "${ARG_WAILS_AMD64_BINARY}"
    ${Else}
        !ifdef SUPPORTS_X86
            File "/oname=${PRODUCT_EXECUTABLE}" "${ARG_RAPIDPROXY_386_BINARY}"
        !endif
    ${EndIf}

    CreateShortcut "$SMPROGRAMS\${INFO_PRODUCTNAME}.lnk" "$INSTDIR\${PRODUCT_EXECUTABLE}"
    CreateShortCut "$DESKTOP\${INFO_PRODUCTNAME}.lnk" "$INSTDIR\${PRODUCT_EXECUTABLE}"

    !insertmacro wails.associateFiles
    !insertmacro wails.associateCustomProtocols

    !insertmacro wails.writeUninstaller
SectionEnd

Section "uninstall"
    # 与安装侧一致：固定全机上下文（卸载器已随安装提权）。
    SetShellVarContext all

    # 清理应用写入的开机自启动条目（HKCU Run 键，见 internal/autostart）
    DeleteRegValue HKCU "Software\Microsoft\Windows\CurrentVersion\Run" "RapidProxy"

    RMDir /r "$AppData\${PRODUCT_EXECUTABLE}" # Remove the WebView2 DataPath

    RMDir /r $INSTDIR

    Delete "$SMPROGRAMS\${INFO_PRODUCTNAME}.lnk"
    Delete "$DESKTOP\${INFO_PRODUCTNAME}.lnk"

    !insertmacro wails.unassociateFiles
    !insertmacro wails.unassociateCustomProtocols

    !insertmacro wails.deleteUninstaller
SectionEnd
