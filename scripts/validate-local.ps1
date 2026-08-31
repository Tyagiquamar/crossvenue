#!/usr/bin/env pwsh
# CrossVenue local validation proof: reproducible, offline, paper-only.
# Runs static checks, tests, race detector, deterministic replay parity,
# a live synthetic engine probe, the failure-scene suite, and a Docker
# build, then writes artifacts/validation/validation.json.
param(
  [switch]$SkipDocker,
  [switch]$Compact # print only the final proof block (make proof)
)
$ErrorActionPreference = 'Continue'
Set-Location (Split-Path $PSScriptRoot -Parent)

$art = 'artifacts/validation'
New-Item -ItemType Directory -Force $art | Out-Null
$script:failed = $false
$script:report = [ordered]@{}

function Step([string]$name, [scriptblock]$body) {
  if (-not $Compact) { Write-Host "== $name" }
  $out = & $body 2>&1
  $code = $LASTEXITCODE
  if (-not $Compact -and $out) { $out | ForEach-Object { Write-Host "   $_" } }
  $ok = ($code -eq 0)
  if (-not $ok) { $script:failed = $true }
  $script:report[$name] = $(if ($ok) { 'pass' } else { 'fail' })
  if (-not $Compact) { Write-Host ("   -> {0}" -f $(if ($ok) { 'PASS' } else { 'FAIL' })) }
}

# ---------- A. environment ----------
$goVersion = (go version) -replace '^go version ', ''
$osArch = "$(go env GOOS)/$(go env GOARCH)"
$commit = (git rev-parse HEAD).Trim()
$shortCommit = $commit.Substring(0, 7)
$timestamp = [DateTime]::UtcNow.ToString('yyyy-MM-ddTHH:mm:ssZ')
if (-not $Compact) {
  Write-Host "go:        $goVersion"
  Write-Host "os/arch:   $osArch"
  Write-Host "commit:    $commit"
  Write-Host "utc:       $timestamp"
}

# ---------- B. static verification ----------
Step 'build' { go build -o bin/crossvenue.exe ./cmd/crossvenue; go build -o bin/replay.exe ./cmd/replay; go build -o bin/loadgen.exe ./cmd/loadgen }
Step 'gofmt' { $bad = gofmt -l .; if ($bad) { $bad; exit 1 } }
Step 'go vet' { go vet ./... }
Step 'staticcheck' { go run honnef.co/go/tools/cmd/staticcheck@2025.1 ./... }
Step 'go test' { go test ./... }
Step 'go test -race' { go test -race ./... }

# ---------- C. deterministic replay parity (seed 42) ----------
$digest1 = ''; $digest2 = ''; $parity = $false
$rec = Join-Path $art 'parity.recording'
if (-not $Compact) { Write-Host '== replay parity (seed 42)' }
./bin/loadgen.exe --venues 3 --symbols 1 --events-per-second 200 --duration 3s --seed 42 --out $rec | Out-Null
./bin/replay.exe --recording $rec --speed max > (Join-Path $art 'replay-run1.txt')
./bin/replay.exe --recording $rec --speed max > (Join-Path $art 'replay-run2.txt')
$digest1 = (Get-FileHash (Join-Path $art 'replay-run1.txt') -Algorithm SHA256).Hash.ToLower()
$digest2 = (Get-FileHash (Join-Path $art 'replay-run2.txt') -Algorithm SHA256).Hash.ToLower()
$parity = ($digest1 -eq $digest2)
if (-not $parity) { $script:failed = $true }
if (-not $Compact) {
  Write-Host "   run1: $digest1"
  Write-Host "   run2: $digest2"
  Write-Host ("   -> {0}" -f $(if ($parity) { 'PASS' } else { 'FAIL' }))
}

# ---------- D. synthetic system proof ----------
$port = 0
foreach ($p in 8477, 8481, 8491, 8501) {
  if (-not (Get-NetTCPConnection -LocalPort $p -State Listen -ErrorAction SilentlyContinue)) { $port = $p; break }
}
if ($port -eq 0) { Write-Host 'no free validation port'; exit 1 }
$base = "http://127.0.0.1:$port"
$env:CROSSVENUE_API_LISTEN = "127.0.0.1:$port"
$env:CROSSVENUE_POSTGRES_URL = ''
$proc = Start-Process -FilePath './bin/crossvenue.exe' -ArgumentList '--mode','synthetic','--config','config.example.yaml' `
  -RedirectStandardOutput (Join-Path $art 'synthetic.log') -RedirectStandardError (Join-Path $art 'synthetic.err.log') -PassThru
$apiReady = $false
$deadline = [DateTime]::Now.AddSeconds(90)
while ([DateTime]::Now -lt $deadline) {
  try {
    $r = Invoke-RestMethod -Uri "$base/ready" -TimeoutSec 2
    if ($r.status -eq 'ready') { $apiReady = $true; break }
  } catch { Start-Sleep -Milliseconds 500 }
}
$venuesReady = 0; $booksReady = 0; $books = @(); $risk = $null
if ($apiReady) {
  foreach ($ep in 'health','ready','api/v1/venues','api/v1/books','api/v1/opportunities','api/v1/balances','api/v1/risk') {
    $name = ($ep -replace '/', '-')
    Invoke-RestMethod -Uri "$base/$ep" -TimeoutSec 5 | ConvertTo-Json -Depth 8 | Set-Content (Join-Path $art "$name.json")
  }
  $books = Get-Content (Join-Path $art 'api-v1-books.json') -Raw | ConvertFrom-Json
  $venues = Get-Content (Join-Path $art 'api-v1-venues.json') -Raw | ConvertFrom-Json
  $risk = Get-Content (Join-Path $art 'api-v1-risk.json') -Raw | ConvertFrom-Json
  $booksReady = @($books | Where-Object { $_.ready -and -not $_.stale }).Count
  $venuesReady = @($venues | Where-Object { $_.connected }).Count
}
if (-not $apiReady) { $script:failed = $true }
# Stop the engine: taskkill kills the whole process tree (a blocked WS or
# lane goroutine can keep the wrapper PID alive under Stop-Process).
& taskkill /PID $proc.Id /T /F 2>&1 | Out-Null
if (-not $Compact) {
  Write-Host ("== synthetic proof: api_ready={0} venues_connected={1} books_ready={2}" -f $apiReady, $venuesReady, $booksReady)
}

# ---------- E. failure-scene suite ----------
$scenes = [ordered]@{
  'sequence gap'      = 'TestScene1SequenceGapInvalidatesAndResyncs'
  'disconnect'        = 'TestScene2DisconnectedVenueExcludedOthersContinue'
  'stale quote'       = 'TestScene3StaleQuoteRejected'
  'partial leg fill'  = 'TestPartialSecondLegFillJournalsResidualExposure'
  'idempotency'       = 'TestScene5DuplicateOrderID'
  'restart recovery'  = 'TestRestartBooksStartInvalid'
  'kill switch'       = 'TestScene7KillSwitchBlocksExecution'
  'queue overload'    = 'TestScene8QueueOverloadInvalidates'
  'live-exec guard'   = 'TestLoadRejectsLiveExecutionEnv'
}
$scenePattern = ($scenes.Values -join '|')
$sceneOut = (go test -count=1 -v -run $scenePattern ./tests/integration/ ./internal/engine/ ./internal/config/ 2>&1 | Out-String)
Set-Content (Join-Path $art 'failure-scenes.txt') $sceneOut
$sceneResults = [ordered]@{}
foreach ($k in $scenes.Keys) {
  $tn = $scenes[$k]
  $pass = ($sceneOut -match "--- PASS: $tn ") -and -not ($sceneOut -match "--- FAIL: $tn")
  $sceneResults[$k] = $pass
  if (-not $pass) { $script:failed = $true }
}
if (-not $Compact) {
  foreach ($k in $scenes.Keys) { Write-Host ("   {0}  {1}" -f $(if ($sceneResults[$k]) { 'PASS' } else { 'FAIL' }), $k) }
}

# ---------- F. docker build ----------
$dockerStatus = 'skip'
if (-not $SkipDocker -and (Get-Command docker -ErrorAction SilentlyContinue)) {
  docker build -q -t crossvenue:validation . 2>&1 | Out-Null
  $dockerStatus = $(if ($LASTEXITCODE -eq 0) { 'pass' } else { 'fail'; $script:failed = $true })
}
if (-not $Compact) { Write-Host "== docker build: $dockerStatus" }

# ---------- validation.json ----------
$validation = [ordered]@{
  commit        = $commit
  go_version    = $goVersion
  os_arch       = $osArch
  timestamp_utc = $timestamp
  tests = [ordered]@{
    unit           = $script:report['go test']
    race           = $script:report['go test -race']
    vet            = $script:report['go vet']
    staticcheck    = $script:report['staticcheck']
    failure_scenes = $(if (($sceneResults.Values | Where-Object { -not $_ }).Count -eq 0) { 'pass' } else { 'fail' })
  }
  replay = [ordered]@{
    seed         = 42
    digest_run_1 = $digest1
    digest_run_2 = $digest2
    identical    = $parity
  }
  synthetic = [ordered]@{
    venues_connected = $venuesReady
    books_ready      = $booksReady
    api_ready        = $apiReady
  }
  docker = [ordered]@{ build = $dockerStatus }
}
$validation | ConvertTo-Json -Depth 6 | Set-Content (Join-Path $art 'validation.json')

# ---------- compact proof block (screenshot-friendly) ----------
Write-Host ''
Write-Host 'CrossVenue validation'
Write-Host "commit: $shortCommit"
Write-Host 'mode: synthetic (paper execution)'
Write-Host ''
Write-Host 'VENUES'
foreach ($b in $books) {
  $state = if ($b.ready -and -not $b.stale) { 'READY' } else { 'NOT-READY' }
  $bid = if ($b.bids) { $b.bids[0].price } else { '-' }
  Write-Host ('{0,-9} {1,-9} {2,-9} bid={3}' -f $b.venue, $state, $b.symbol, $bid)
}
Write-Host ''
Write-Host 'ENGINE'
Write-Host ('books ready:        {0}/{1}' -f $booksReady, @($books).Count)
Write-Host ('api ready:          {0}' -f $apiReady)
$kill = if ($risk) { $risk.kill_switch } else { 'unknown' }
Write-Host ('risk kill switch:   {0}' -f $(if ($kill -eq $false) { 'inactive (ACTIVE risk checks)' } else { $kill }))
Write-Host 'execution:          PAPER ONLY'
Write-Host ''
Write-Host 'REPLAY'
Write-Host 'seed:               42'
Write-Host "digest run 1:       $digest1"
Write-Host "digest run 2:       $digest2"
Write-Host ('parity:             {0}' -f $(if ($parity) { 'PASS' } else { 'FAIL' }))
Write-Host ''
Write-Host 'FAILURE SCENES'
foreach ($k in $scenes.Keys) {
  Write-Host ('{0,-19} {1}' -f "${k}:", $(if ($sceneResults[$k]) { 'PASS' } else { 'FAIL' }))
}
Write-Host ''
Write-Host 'TESTS'
foreach ($t in 'gofmt','go vet','staticcheck','go test','go test -race') {
  Write-Host ('{0,-19} {1}' -f "${t}:", $(if ($script:report[$t] -eq 'pass') { 'PASS' } else { 'FAIL' }))
}
Write-Host ('{0,-19} {1}' -f 'docker build:', $dockerStatus.ToUpper())
Write-Host ''
if ($script:failed) { Write-Host 'RESULT: VALIDATION FAILED'; exit 1 }
Write-Host 'RESULT: ALL LOCAL VALIDATIONS PASSED'
exit 0
