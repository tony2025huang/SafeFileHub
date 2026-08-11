# Transfer benchmark harness

`bench` provides a deliberately local benchmark harness for transfer scheduling.
`Run` starts a fixed number of workers and feeds them through an unbuffered job
channel: transfer count therefore cannot create an unbounded pending goroutine
or application queue.

## Run the guards and benchmark

```bash
GOTOOLCHAIN=local /usr/local/go/bin/go test ./bench -race -count=1
GOTOOLCHAIN=local /usr/local/go/bin/go test ./bench -bench . -benchmem -count=1
```

The benchmark copies a 1 MiB in-memory payload to `io.Discard`; it measures
scheduler/copy overhead only, not network, TLS, HTTP/2, disk, or NAS
throughput. The guard starts 16 blocked upload requests and confirms that a
separate `/healthz` request completes within 250 ms.

For an environment baseline, record the exact host, peer, protocol, RTT,
storage, commands, and raw results under `bench/results/`. Run `iperf3` only
against an explicitly supplied isolated peer; do not infer or fabricate it.
