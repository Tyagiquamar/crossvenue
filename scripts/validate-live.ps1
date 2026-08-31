#!/usr/bin/env pwsh
# CrossVenue LIVE public-feed validation (optional, internet-dependent).
# Uses public unauthenticated WebSocket feeds only. No API keys are read.
# Execution remains paper/simulated; this script never places orders.
# Zero profitable opportunities is NOT a failure. Unreachable venues are
# reported distinctly and produce a PARTIAL result, never fabricated data.
param(
  [int]$TimeoutSec = 120
)
$ErrorActionPreference = 'Continue'
Set-Location (Split-Path $PSScriptRoot -Parent)

$art = 'artifacts/validation/live'
New-Item -ItemType Directory -Force $art | Out-Null

Write-Host 'CrossVenue live public-feed validation (paper execution only)'
Write-Host "commit: $((git rev-parse --short HEAD).Trim())"
Write-Host "utc:    $([DateTime]::UtcNow.ToString('yyyy-MM-ddTHH:mm:ssZ'))"

# Build if needed.
if (-not (Test-Path 'bin/crossvenue.exe')) {
  go build -o bin/crossvenue.exe ./cmd/crossvenue
  if ($LASTEXITCODE -ne 0) { Write-Host 'build failed'; exit 1 }
}

$port = 0
foreach ($p in 8479, 8483, 8493, 8503) {
  if (-not (Get-NetTCPConnection -LocalPort $p -State Listen -ErrorAction SilentlyContinue)) { $port = $p; break }
}
if ($port -eq 0) { Write-Host 'no free validation port'; exit 1 }
$base = "http://127.0.0.1:$port"
$env:CROSSVENUE_API_LISTEN = "127.0.0.1:$port"
$env:CROSSVENUE_POSTGRES_URL = ''

$proc = Start-Process -FilePath './bin/crossvenue.exe' -ArgumentList '--mode','live-market-sim','--config','config.example.yaml' `
  -RedirectStandardOutput (Join-Path $art 'engine.log') -RedirectStandardError (Join-Path $art 'engine.err.log') -PassThru

# Wait for the API (liveness) first.
$alive = $false
$deadline = [DateTime]::Now.AddSeconds(30)
while ([DateTime]::Now -lt $deadline) {
  try { Invoke-RestMethod -Uri "$base/health" -TimeoutSec 2 | Out-Null; $alive = $true; break } catch { Start-Sleep -Milliseconds 500 }
}
if (-not $alive) {
  Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue
  Write-Host 'RESULT: FAIL (engine API never became live)'
  exit 1
}

# Give venues time to connect and synchronize books.
Write-Host "waiting up to ${TimeoutSec}s for venue feeds..."
$deadline = [DateTime]::Now.AddSeconds($TimeoutSec)
$venues = @(); $books = @()
while ([DateTime]::Now -lt $deadline) {
  try {
    $venues = @(Invoke-RestMethod -Uri "$base/api/v1/venues" -TimeoutSec 5)
    $books = @(Invoke-RestMethod -Uri "$base/api/v1/books" -TimeoutSec 5)
    $readyCount = @($books | Where-Object { $_.ready -and -not $_.stale }).Count
    if ($readyCount -ge 3) { break }
  } catch { }
  Start-Sleep -Seconds 2
}

$venues | ConvertTo-Json -Depth 6 | Set-Content (Join-Path $art 'venues.json')
$books | ConvertTo-Json -Depth 8 | Set-Content (Join-Path $art 'books.json')
try { Invoke-RestMethod -Uri "$base/api/v1/opportunities" -TimeoutSec 5 | ConvertTo-Json -Depth 8 | Set-Content (Join-Path $art 'opportunities.json') } catch { '[]' | Set-Content (Join-Path $art 'opportunities.json') }
$opps = @(Get-Content (Join-Path $art 'opportunities.json') -Raw | ConvertFrom-Json)

Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue

Write-Host ''
Write-Host ('{0,-9} {1,-11} {2,-11} {3,-14} {4,-14} {5,-14} {6,-8} {7}' -f 'VENUE','CONNECTED','BOOK READY','SEQ','BEST BID','BEST ASK','AGE MS','GAPS')
$synced = 0; $connected = 0
foreach ($v in ($venues | Sort-Object venue)) {
  $b = $books | Where-Object { $_.venue -eq $v.venue } | Select-Object -First 1
  $bookReady = $b -and $b.ready -and -not $b.stale
  if ($v.connected) { $connected++ }
  if ($v.connected -and $bookReady -and $v.sequence_gaps -eq 0) { $synced++ }
  # Coerce possibly-array JSON scalars to single values for clean display.
  $bid = if ($b -and $b.bids) { [string]($b.bids | Select-Object -First 1).price } else { '-' }
  $ask = if ($b -and $b.asks) { [string]($b.asks | Select-Object -First 1).price } else { '-' }
  $seq = if ($b) { [string]$b.sequence } else { '-' }
  $age = if ($b) { [string]$b.age_ms } else { '-' }
  $gaps = [string]$v.sequence_gaps
  $state = if ($v.connected) { 'yes' } else { 'NO (unreachable)' }
  Write-Host ('{0,-9} {1,-11} {2,-11} {3,-14} {4,-14} {5,-14} {6,-8} {7}' -f $v.venue, $state, $(if ($bookReady) { 'yes' } else { 'no' }), $seq, $bid, $ask, $age, $gaps)
}
Write-Host ''
Write-Host "opportunities detected: $($opps.Count) (zero is fine — spreads after fees are rare)"
Write-Host 'execution: PAPER ONLY (no order endpoints exist; no API keys read)'

if ($synced -ge 3) { Write-Host 'RESULT: PASS (3/3 venues connected, books synchronized, no sequence corruption)'; exit 0 }
if ($synced -ge 1) { Write-Host "RESULT: PARTIAL ($synced/3 venues synchronized; see table for unreachable venues)"; exit 0 }
Write-Host 'RESULT: FAIL (no venue synchronized)'
exit 1
