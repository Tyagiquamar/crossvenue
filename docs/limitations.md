# Limitations

- **Public market data only** by default. No private feeds, no colocation,
  no exchange-side latency guarantees.
- **Simulated fills cannot reproduce queue position.** The simulator
  consumes visible depth; real taker fills would additionally face
  competition and could be worse. Maker behavior is not modeled.
- **Modeled latency is a penalty parameter**, not a measurement of real
  exchange execution latency.
- **No cross-venue atomicity.** Two-leg fills are independent; residual
  exposure is real risk in live trading and is modeled here as such.
- **Asset transfer between venues is not modeled as instant.** Inventory
  is per venue; rebalancing is out of scope.
- **Clock offsets**: exchange timestamps are not synchronized with local
  time. `ReceiveTime - ExchangeTime` is labeled as clock-dependent; the
  reliable internal metric is `DecisionTime - ReceiveTime`.
- **No profitability claims.** Configured fees are parameters; observed
  spreads frequently disappear after costs. Nothing here is evidence of a
  profitable strategy.
- **Not HFT.** Benchmarks report measured numbers only, on the machine
  they were measured on.
- **Live execution is not implemented.** The binary refuses to start if
  `ENABLE_LIVE_EXECUTION=true`.
