# Builds dist\pg.exe from cmd\pgide, with its icon, version and manifest, and
# with -Installer, dist\pg-setup.exe too, which needs NSIS (winget install NSIS.NSIS).
#
#   powershell -ExecutionPolicy Bypass -File scripts\build-windows.ps1 [-Installer]
#
# It needs a C compiler for cgo, as pg_query is C: MinGW-w64's gcc on the PATH,
# such as MSYS2's UCRT64 one (pacman -S mingw-w64-ucrt-x86_64-gcc).
#
# VERSION is the release tag without its "v"; "dev" builds don't update.
# REPOSITORY is the GitHub repository updates come from, as owner/name.
param(
	[string]$Version = $(if ($env:VERSION) { $env:VERSION } else { 'dev' }),
	[string]$Repository = $env:REPOSITORY,
	[switch]$Installer
)
$ErrorActionPreference = 'Stop'
Set-Location (Join-Path $PSScriptRoot '..')

# Windows wants numbers for the file's version: 1.2.3 for 1.2.3-beta, and
# 0.0.0 for dev builds.
$numeric = '0.0.0'
if ($Version -match '^(\d+)(?:\.(\d+))?(?:\.(\d+))?') {
	$numeric = '{0}.{1}.{2}' -f [int]$Matches[1], [int]$Matches[2], [int]$Matches[3]
}

New-Item -ItemType Directory -Force dist | Out-Null
$syso = 'cmd\pgide\rsrc_windows_amd64.syso'
try {
	# The icon, version details and manifest go in as resources. The manifest
	# turns on the modern common controls, for dialogs, and sharp text on
	# high-DPI screens.
	go run github.com/tc-hib/go-winres@v0.3.3 simply `
		--arch amd64 --out cmd\pgide\rsrc `
		--icon scripts\windows\icon.ico --manifest gui `
		--product-name pg --file-description pg --original-filename pg.exe `
		--product-version $numeric --file-version $numeric
	if ($LASTEXITCODE) { throw "go-winres failed" }

	$ldflags = "-s -w -H windowsgui -X main.version=$Version -extldflags=-static"
	if ($Repository) { $ldflags += " -X main.repository=$Repository" }
	$env:CGO_ENABLED = '1'
	go build -trimpath -ldflags $ldflags -o dist\pg.exe .\cmd\pgide
	if ($LASTEXITCODE) { throw "go build failed" }
} finally {
	Remove-Item -ErrorAction SilentlyContinue $syso
}
Write-Host "built dist\pg.exe"

if ($Installer) {
	$makensis = (Get-Command makensis -ErrorAction SilentlyContinue).Source
	if (-not $makensis) {
		$makensis = Join-Path ${env:ProgramFiles(x86)} 'NSIS\makensis.exe'
	}
	if (-not (Test-Path $makensis)) { throw "makensis not found: install NSIS (winget install NSIS.NSIS)" }
	& $makensis /V2 "/DVERSION=$numeric" scripts\windows\installer.nsi
	if ($LASTEXITCODE) { throw "makensis failed" }
	Write-Host "built dist\pg-setup.exe"
}
