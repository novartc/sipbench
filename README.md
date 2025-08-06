# SIPBench

A high-performance SIP (Session Initiation Protocol) benchmarking tool that integrates eBPF/XDP to implement kernel-level packet processing for RTP media.

It provides User Agent Client (UAC), User Agent Server (UAS) and SIPREC Session Recording Server (SRS) functions. It can support **8k** concurrent sessions (including media) on a single machine.

## Installation

Binary can be downloaded from the [Releases](https://github.com/novartc/sipbench/releases) page.

## Usage

### UAC (Client)

```bash
./sipbench uac [options] <destination>
```

Example:
```bash
# Generate calls to SIP server at 10.0.0.2:8626
sudo ./sipbench uac --interface veth2 --address 10.0.0.3 --port 5060 \
  --rate 2 --max-calls 10 --total-calls 100 \
  --user 9196 10.0.0.2:8626
```

**Example Explanation:**
- `--interface veth2`: Uses the veth2 network interface
- `--address 10.0.0.3`: Binds the UAC to local IP address 10.0.0.3
- `--port 5060`: Uses port 5060 for SIP signaling
- `--rate 2`: Generates 2 new calls per second
- `--max-calls 10`: Maintains maximum of 10 concurrent active calls
- `--total-calls 100`: Stops after making 100 total calls
- `--user 9196`: Sets the SIP user identifier to 9196 for outgoing calls
- `10.0.0.2:8626`: Target SIP server address and port to send calls to

### UAS (Server)

```bash
./sipbench uas [options]
```

Example:
```bash
# Listen on veth2, port 5060
sudo ./sipbench uas --interface veth2 --address 10.0.0.3 --port 5060 \
  --ringing-timeout 5 --answer-timeout 20 --answer-chance 10 \
  --early-media 30 --answer-media 30
```

**Example Explanation:**
- `--interface veth2`: Uses the veth2 network interface
- `--address 10.0.0.3`: Listens on IP address 10.0.0.3
- `--port 5060`: Listens on port 5060 for SIP signaling
- `--ringing-timeout 5`: Rings for 5 seconds before proceeding to answer decision
- `--answer-timeout 20`: Waits up to 20 seconds before hang up the call
- `--answer-chance 10`: Only answers 10% of incoming calls
- `--early-media 30`: Plays 30 seconds of white noise as early media during ringing
- `--answer-media 30`: Plays 30 seconds of white noise as media after answering calls

### Configuration Options

#### UAC Options
- `--interface`: Network interface name for eBPF/XDP binding (default: "eth0")
- `--address`: Local bind address for SIP signaling
- `--port`: Local bind port for SIP signaling (default: 5060)
- `--rate`: Call rate in calls per second (default: 10)
- `--max-calls`: Maximum number of concurrent calls (default: 1)
- `--total-calls`: Total number of calls to make (default: 0, unlimited)
- `--user`: SIP remote user identifier
- `--min-rtp-port`: Minimum RTP port range (default: 20000)
- `--max-rtp-port`: Maximum RTP port range (default: 60000)
- `--ringing-timeout`: Ringing timeout in seconds (default: "5")
- `--answer-timeout`: Answer timeout in seconds (default: "20")
- `--early-media`: Early media content - white noise duration, pcap file, or pcm file (default: "30")
- `--answer-media`: Answer media content - white noise duration, pcap file, or pcm file (default: "30")
- `--xdp-size`: XDP ring buffer size, must be power of 2 (default: 4096)

#### UAS Options
- `--interface`: Network interface name for eBPF/XDP binding (default: "eth0")
- `--address`: Listen address for SIP signaling
- `--port`: Listen port for SIP signaling (default: 5060)
- `--public-addr`: NAT/public address for SIP Contact header (defaults to --address)
- `--answer-chance`: Probability of answering calls (0-100, default: 100)
- `--min-rtp-port`: Minimum RTP port range (default: 20000)
- `--max-rtp-port`: Maximum RTP port range (default: 60000)
- `--ringing-timeout`: Ringing timeout in seconds (default: "5")
- `--answer-timeout`: Answer timeout in seconds (default: "20")
- `--early-media`: Early media content - white noise duration, pcap file, or pcm file (default: "30")
- `--answer-media`: Answer media content - white noise duration, pcap file, or pcm file (default: "30")
- `--xdp-size`: XDP ring buffer size, must be power of 2 (default: 4096)

## Troubleshooting

### Common Issues

1. **eBPF Loading Failures**: Ensure kernel supports eBPF/XDP and you have root privileges
