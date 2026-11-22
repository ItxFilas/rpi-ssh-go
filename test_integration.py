import argparse
import base64
import ctypes
from ctypes import wintypes
import hashlib
import json
import os
import statistics
import subprocess
import sys
import time

def get_git_revision(repo_dir):
    try:
        out = subprocess.check_output(
            ["git", "rev-parse", "HEAD"],
            cwd=repo_dir,
            stderr=subprocess.DEVNULL
        )
        return out.decode("utf-8").strip()
    except Exception:
        return "unknown"

def get_go_version():
    try:
        out = subprocess.check_output(
            ["go", "version"],
            stderr=subprocess.DEVNULL
        )
        return out.decode("utf-8").strip()
    except Exception:
        return "unknown"

def get_file_sha256(file_path):
    h = hashlib.sha256()
    with open(file_path, "rb") as f:
        while chunk := f.read(65536):
            h.update(chunk)
    return h.hexdigest().upper()

def get_process_memory_mb(pid):
    class PROCESS_MEMORY_COUNTERS_EX(ctypes.Structure):
        _fields_ = [
            ("cb", wintypes.DWORD),
            ("PageFaultCount", wintypes.DWORD),
            ("PeakWorkingSetSize", ctypes.c_size_t),
            ("WorkingSetSize", ctypes.c_size_t),
            ("QuotaPeakPagedPoolUsage", ctypes.c_size_t),
            ("QuotaPagedPoolUsage", ctypes.c_size_t),
            ("QuotaPeakNonPagedPoolUsage", ctypes.c_size_t),
            ("QuotaNonPagedPoolUsage", ctypes.c_size_t),
            ("PagefileUsage", ctypes.c_size_t),
            ("PeakPagefileUsage", ctypes.c_size_t),
            ("PrivateUsage", ctypes.c_size_t),
        ]
    PROCESS_QUERY_INFORMATION = 0x0400
    PROCESS_VM_READ = 0x0010
    handle = ctypes.windll.kernel32.OpenProcess(PROCESS_QUERY_INFORMATION | PROCESS_VM_READ, False, pid)
    if not handle:
        return 0.0, 0.0, 0.0
    try:
        counters = PROCESS_MEMORY_COUNTERS_EX()
        counters.cb = ctypes.sizeof(PROCESS_MEMORY_COUNTERS_EX)
        if ctypes.windll.psapi.GetProcessMemoryInfo(handle, ctypes.byref(counters), counters.cb):
            ws_mb = counters.WorkingSetSize / (1024 * 1024)
            priv_mb = counters.PrivateUsage / (1024 * 1024)
            peak_mb = counters.PeakWorkingSetSize / (1024 * 1024)
            return ws_mb, priv_mb, peak_mb
        return 0.0, 0.0, 0.0
    finally:
        ctypes.windll.kernel32.CloseHandle(handle)

def send_rpc(proc, method, params=None, req_id=1):
    payload = {
        "jsonrpc": "2.0",
        "id": req_id,
        "method": method,
        "params": params or {}
    }
    line = json.dumps(payload, ensure_ascii=False) + "\n"
    proc.stdin.write(line.encode("utf-8"))
    proc.stdin.flush()
    resp_line = proc.stdout.readline().decode("utf-8")
    if not resp_line:
        stderr_output = ""
        try:
            if proc.poll() is not None and proc.stderr:
                stderr_output = proc.stderr.read().decode("utf-8", errors="replace")
        except Exception:
            pass
        raise RuntimeError(f"Empty response from server (method={method}, id={req_id}). stderr: {stderr_output}")
    try:
        return json.loads(resp_line.strip())
    except (json.JSONDecodeError, TypeError) as e:
        raise RuntimeError(f"Malformed JSON response from server (method={method}, id={req_id}): {e}\nRaw: {resp_line.strip()[:200]}")

def send_notification(proc, method, params=None):
    payload = {
        "jsonrpc": "2.0",
        "method": method,
        "params": params or {}
    }
    line = json.dumps(payload, ensure_ascii=False) + "\n"
    proc.stdin.write(line.encode("utf-8"))
    proc.stdin.flush()

def call_tool(proc, tool_name, arguments, req_id):
    resp = send_rpc(proc, "tools/call", {
        "name": tool_name,
        "arguments": arguments
    }, req_id)
    if resp.get("error"):
        raise RuntimeError(f"RPC error calling '{tool_name}' (id={req_id}): {resp['error']}")
    result = resp.get("result", {})
    if result.get("isError"):
        content_items = result.get("content", [])
        err_msg = content_items[0].get("text", "") if content_items else str(result)
        raise RuntimeError(f"Tool error from '{tool_name}' (id={req_id}): {err_msg}")
    content_items = result.get("content", [])
    if not content_items:
        return ""
    return content_items[0].get("text", "")

def call_tool_json(proc, tool_name, arguments, req_id):
    raw_text = call_tool(proc, tool_name, arguments, req_id)
    try:
        return json.loads(raw_text)
    except (json.JSONDecodeError, TypeError) as e:
        raise RuntimeError(f"Failed to decode JSON from '{tool_name}' (id={req_id}): {e}\nRaw output: {raw_text[:200]}")

def create_fixture(size_bytes):
    """
    Generate test content of exactly size_bytes.
    For sizes up to 64 KiB, standard ASCII text is used.
    For 1 MiB, multi-byte UTF-8 characters ('\\u00e9') are used so the rune count
    (524,288 runes) stays well within the server's 1,000,000 max_chars limit,
    preventing unintended truncation while rigorously exercising the full 1 MiB payload.
    """
    if size_bytes <= 65536:
        base = "Raspberry Pi Connect Go MCP Server Integration Fixture Payload Line.\n"
        reps = (size_bytes // len(base)) + 1
        content = (base * reps)[:size_bytes]
        assert len(content.encode("utf-8")) == size_bytes
        return content
    else:
        char = "\u00e9"
        char_bytes = char.encode("utf-8")  # 2 bytes
        num_chars = size_bytes // len(char_bytes)
        rem = size_bytes % len(char_bytes)
        content = (char * num_chars) + ("X" * rem)
        assert len(content.encode("utf-8")) == size_bytes
        return content

def parse_args():
    parser = argparse.ArgumentParser(
        description="Raspberry Pi Connect Go MCP Server: End-to-End Integration & Benchmark Harness"
    )
    parser.add_argument("--max-working-set-mb", type=float, default=None, help="Optional explicit working-set budget; otherwise report measured memory without a pass/fail claim")
    parser.add_argument(
        "--binary",
        dest="binary",
        default=None,
        help="Path to rpi-ssh.exe binary (default: searches dist/rpi-ssh.exe, ./rpi-ssh.exe, rpi-ssh.exe)"
    )
    parser.add_argument(
        "--device",
        dest="device",
        default=None,
        help="Optional target device name or ID (default: auto-discovers from ssh_list_devices)"
    )
    parser.add_argument(
        "--allow-write-tests",
        dest="allow_write_tests",
        action="store_true",
        default=False,
        help="Allow file upload/download write tests (default: False, read-only tests only)"
    )
    parser.add_argument(
        "--upload-1mb-runs",
        dest="upload_1mb_runs",
        type=int,
        default=3,
        help="Number of 1 MiB benchmark repetitions to measure median throughput (default: 3)"
    )
    parser.add_argument(
        "--upload-8mb-runs",
        dest="upload_8mb_runs",
        type=int,
        default=1,
        help="Number of 8 MiB benchmark repetitions (default: 1)"
    )
    parser.add_argument(
        "--repeat-1mb-soak",
        dest="repeat_1mb_soak",
        action="store_true",
        default=False,
        help="Run 10 repeated 1 MiB uploads to measure memory stability before/after (default: False)"
    )
    parser.add_argument(
        "--cold-iterations",
        dest="cold_iterations",
        type=int,
        default=3,
        help="Number of cold connection benchmark iterations (default: 3)"
    )
    parser.add_argument(
        "--warm-iterations",
        dest="warm_iterations",
        type=int,
        default=100,
        help="Number of warm command benchmark iterations (default: 100)"
    )
    return parser.parse_args()

def main():
    if hasattr(sys.stdout, "reconfigure"):
        sys.stdout.reconfigure(encoding="utf-8")

    args = parse_args()
    repo_dir = os.path.dirname(os.path.abspath(__file__))

    # Resolve binary path
    binary_path = args.binary
    if not binary_path:
        for candidate in [
            os.path.join(repo_dir, "dist", "rpi-ssh.exe"),
            os.path.join(repo_dir, "rpi-ssh.exe"),
            "rpi-ssh.exe",
        ]:
            if os.path.isfile(candidate):
                binary_path = candidate
                break
    if not binary_path:
        binary_path = os.path.join(repo_dir, "dist", "rpi-ssh.exe")

    binary_path = os.path.abspath(binary_path)
    if not os.path.isfile(binary_path):
        raise FileNotFoundError(
            f"Binary not found at '{binary_path}'. Please build it first via scripts/build.ps1 or specify --binary."
        )

    git_rev = get_git_revision(repo_dir)
    binary_sha = get_file_sha256(binary_path)
    go_ver = get_go_version()
    network_mode = os.environ.get("RPI_NETWORK_MODE", "outbound-relay (default)")

    # Print harness banner
    print("=" * 75)
    print("Raspberry Pi Connect Go MCP Server: Integration & Benchmark Harness")
    print(f"Source Revision : {git_rev}")
    print(f"Binary Path     : {binary_path}")
    print(f"Binary SHA-256  : {binary_sha}")
    print(f"Go Version      : {go_ver}")
    print(f"Network Mode    : {network_mode}")
    print(f"Allow Write     : {args.allow_write_tests}")
    print(f"1 MiB Reps      : {args.upload_1mb_runs}")
    print(f"8 MiB Reps      : {args.upload_8mb_runs}")
    print(f"1 MiB Soak 10x  : {args.repeat_1mb_soak}")
    print(f"Cold Iterations : {args.cold_iterations}")
    print(f"Warm Iterations : {args.warm_iterations}")
    print("=" * 75)

    proc_cwd = os.path.dirname(binary_path)
    proc = subprocess.Popen(
        [binary_path],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        cwd=proc_cwd
    )

    try:
        # 1. Initialize
        t0 = time.perf_counter()
        init_resp = send_rpc(proc, "initialize", {
            "protocolVersion": "2024-11-05",
            "clientInfo": {"name": "integration-tester", "version": "2.0.0"}
        }, 1)
        init_lat = (time.perf_counter() - t0) * 1000
        server_info = init_resp.get("result", {}).get("serverInfo", {})
        print(f"[1] MCP Initialize: {init_lat:.2f}ms | Server: {server_info.get('name', 'unknown')} v{server_info.get('version', 'unknown')}")
        send_notification(proc, "notifications/initialized")

        # 2. List tools
        tools_resp = send_rpc(proc, "tools/list", {}, 2)
        tools = [t["name"] for t in tools_resp.get("result", {}).get("tools", [])]
        print(f"[2] Registered Tools ({len(tools)}): {tools}")
        expected_tools = {"ssh_list_devices", "ssh_exec", "ssh_upload_file", "ssh_download_file", "ssh_disconnect"}
        assert expected_tools.issubset(set(tools)), f"Missing expected tools: {expected_tools - set(tools)}"

        # 3. List devices and resolve target device
        t0 = time.perf_counter()
        dev_json = call_tool_json(proc, "ssh_list_devices", {}, 3)
        dev_lat = (time.perf_counter() - t0) * 1000
        print(f"[3] Device Listing: {dev_lat:.2f}ms")
        devices = dev_json.get("devices", [])

        # Determine target device: user-specified or auto-discovered
        target_device = args.device
        if target_device:
            matched = any(d.get("name") == target_device or d.get("id") == target_device for d in devices)
            if not matched and devices:
                print(f"    WARNING: Requested device '{target_device}' was not found in devices list.")
        else:
            online_shell = [d["name"] for d in devices if d.get("online") and d.get("shell")]
            online_any = [d["name"] for d in devices if d.get("online")]
            if online_shell:
                target_device = online_shell[0]
                print(f"    Auto-discovered target device (online+shell): {target_device}")
            elif online_any:
                target_device = online_any[0]
                print(f"    Auto-discovered target device (online): {target_device}")
            elif devices:
                target_device = devices[0]["name"]
                print(f"    Auto-discovered target device (first listed): {target_device}")
            else:
                raise SystemExit("No devices found on the account; pass --device <name>")

        print(f"    Target Device: {target_device}")

        # 4. Cold Connection Benchmark (multiple WebRTC establishment + bash session cycles)
        cold_latencies = []
        num_cold = max(1, args.cold_iterations)
        print(f"\n[4] Running {num_cold} cold connection cycle(s)...")
        for i in range(num_cold):
            t0 = time.perf_counter()
            cold_out = call_tool_json(proc, "ssh_exec", {
                "device_name": target_device,
                "command": "uname -a"
            }, 50 + i)
            dur = (time.perf_counter() - t0) * 1000
            cold_latencies.append(dur)
            uname_str = cold_out.get("stdout", "").strip()
            assert cold_out.get("exit_code") == 0, f"Cold exec failed on cycle {i + 1}: {cold_out}"
            print(f"    Cycle {i + 1}/{num_cold}: {dur:.2f}ms | uname: {uname_str[:50]}...")

            # Disconnect after cycle so next cycle is cold, EXCEPT after the last cycle
            # so the session remains established and warm for subsequent benchmarks.
            if i < num_cold - 1:
                disc_resp = call_tool(proc, "ssh_disconnect", {"device_name": target_device}, 60 + i)
                assert "Disconnected" in disc_resp or "No active session" in disc_resp

        sorted_cold = sorted(cold_latencies)
        median_cold = statistics.median(sorted_cold)
        p95_cold = sorted_cold[min(int(len(sorted_cold) * 0.95), len(sorted_cold) - 1)]
        min_cold = sorted_cold[0]
        max_cold = sorted_cold[-1]
        print(f"[4] Cold Connection Latency ({num_cold} cycle(s)):")
        print(f"    Min: {min_cold:.2f}ms | Median: {median_cold:.2f}ms | p95: {p95_cold:.2f}ms | Max: {max_cold:.2f}ms")

        # 5. Warm Command Latency Benchmark
        warm_latencies = []
        num_warm = max(1, args.warm_iterations)
        print(f"\n[5] Running {num_warm} warm command iterations...")
        for i in range(num_warm):
            t0 = time.perf_counter()
            out = call_tool_json(proc, "ssh_exec", {
                "device_name": target_device,
                "command": "echo warm-ping"
            }, 1000 + i)
            dur = (time.perf_counter() - t0) * 1000
            warm_latencies.append(dur)
            assert out.get("exit_code") == 0, f"Non-zero exit on warm-ping iteration {i}: {out}"
            assert out.get("stdout") == "warm-ping", "Shell prompts or command echoes leaked into stdout"

        sorted_latencies = sorted(warm_latencies)
        median_warm = statistics.median(sorted_latencies)
        p95_warm = sorted_latencies[min(int(len(sorted_latencies) * 0.95), len(sorted_latencies) - 1)]
        min_warm = sorted_latencies[0]
        max_warm = sorted_latencies[-1]
        print(f"[5] Warm Execution Latency ({num_warm} runs):")
        print(f"    Min: {min_warm:.2f}ms | Median: {median_warm:.2f}ms | p95: {p95_warm:.2f}ms | Max: {max_warm:.2f}ms")

        # 6. Complex escaping & python/subshell execution
        t0 = time.perf_counter()
        esc_out = call_tool_json(proc, "ssh_exec", {
            "device_name": target_device,
            "command": "python3 -c \"import sys; print('python-ok', sys.version.split()[0])\""
        }, 2000)
        esc_lat = (time.perf_counter() - t0) * 1000
        print(f"\n[6] Quoted Execution: {esc_lat:.2f}ms | Output: {esc_out.get('stdout', '').strip()} (exit: {esc_out.get('exit_code')})")
        assert esc_out.get("exit_code") == 0
        assert esc_out.get("stdout", "").startswith("python-ok ") and "\n" not in esc_out["stdout"], "Contaminated quoted command output"

        # 7. Non-zero exit code capture test
        exit_out = call_tool_json(proc, "ssh_exec", {
            "device_name": target_device,
            "command": "sh -c 'exit 42'"
        }, 2001)
        print(f"[7] Non-Zero Exit Code Capture: {exit_out.get('exit_code')} (Expected: 42)")
        assert exit_out.get("exit_code") == 42

        # 8. Multi-tier Write Tests OR Read-only alternative
        if not args.allow_write_tests:
            print("\n[8] Skipping file upload/download write tests (--allow-write-tests is False).")
            print("    Running read-only commands instead:")

            # Read-only check 1: /proc/uptime
            t0 = time.perf_counter()
            ro_out = call_tool_json(proc, "ssh_exec", {
                "device_name": target_device,
                "command": "cat /proc/uptime"
            }, 3000)
            ro_lat = (time.perf_counter() - t0) * 1000
            print(f"    [RO-1] cat /proc/uptime: {ro_lat:.2f}ms | Output: {ro_out.get('stdout', '').strip()}")
            assert ro_out.get("exit_code") == 0

            # Read-only check 2: echo ping
            t0 = time.perf_counter()
            ro_out2 = call_tool_json(proc, "ssh_exec", {
                "device_name": target_device,
                "command": "echo 'read-only-verification-ok'"
            }, 3001)
            ro_lat2 = (time.perf_counter() - t0) * 1000
            print(f"    [RO-2] echo check: {ro_lat2:.2f}ms | Output: {ro_out2.get('stdout', '').strip()}")
            assert ro_out2.get("exit_code") == 0
            assert "read-only-verification-ok" in ro_out2.get("stdout", "")
        else:
            print("\n[8] Executing multi-tier file transfer write tests (--allow-write-tests is True):")
            fixtures = [
                ("0 B", 0, 1),
                ("1 KiB", 1024, 1),
                ("64 KiB", 64 * 1024, 1),
                ("1 MiB", 1024 * 1024, args.upload_1mb_runs),
                ("8 MiB", 8 * 1024 * 1024, args.upload_8mb_runs),
            ]
            for idx, (label, size_bytes, reps) in enumerate(fixtures):
                print(f"\n    --- Testing Fixture Tier [{label}] ({size_bytes} bytes, {reps} runs) ---")
                durations = []
                throughputs = []
                for run_idx in range(reps):
                    run_label = f" (run {run_idx+1}/{reps})" if reps > 1 else ""
                    remote_dir = f"/tmp/mcp_test_{size_bytes}_{os.getpid()}_{int(time.time() * 1000)}_{run_idx}"
                    remote_file = f"{remote_dir}/fixture_{label.replace(' ', '_').lower()}.txt"

                    # 1. Create dedicated temporary remote directory
                    call_tool_json(proc, "ssh_exec", {
                        "device_name": target_device,
                        "command": f"mkdir -p '{remote_dir}'"
                    }, 4000 + idx * 100 + run_idx * 10)

                    try:
                        # 2. Generate fixture content and expected SHA-256
                        test_content = create_fixture(size_bytes)
                        test_bytes = test_content.encode("utf-8")
                        assert len(test_bytes) == size_bytes, f"Generated fixture size mismatch: {len(test_bytes)} != {size_bytes}"
                        expected_sha = hashlib.sha256(test_bytes).hexdigest()

                        # 3. Upload fixture
                        t0 = time.perf_counter()
                        up_out = call_tool(proc, "ssh_upload_file", {
                            "device_name": target_device,
                            "remote_path": remote_file,
                            "content": test_content
                        }, 4001 + idx * 100 + run_idx * 10)
                        up_lat = (time.perf_counter() - t0) * 1000
                        up_sec = up_lat / 1000.0
                        durations.append(up_sec)
                        size_kib = size_bytes / 1024.0
                        size_mb = size_bytes / (1024.0 * 1024.0)
                        up_thru_kib = (size_kib / up_sec) if up_sec > 0 else 0
                        up_thru_mb = (size_mb / up_sec) if up_sec > 0 else 0
                        throughputs.append(up_thru_mb if size_bytes >= 1024 * 1024 else up_thru_kib)
                        thru_str_up = f"{up_thru_mb:.2f} MB/s" if size_bytes >= 1024 * 1024 else f"{up_thru_kib:.1f} KiB/s"
                        print(f"    Upload ({label}{run_label}): {up_lat:.2f}ms ({thru_str_up}) | Result: {up_out.strip()}")
                        assert "Successfully uploaded" in up_out

                        # 4. Verify remote SHA-256 integrity directly via sha256sum
                        sha_out = call_tool_json(proc, "ssh_exec", {
                            "device_name": target_device,
                            "command": f"sha256sum -- '{remote_file}'"
                        }, 4002 + idx * 100 + run_idx * 10)
                        fields = sha_out.get("stdout", "").strip().split()
                        remote_sha = fields[0] if fields else ""
                        assert remote_sha.lower() == expected_sha.lower(), f"Remote sha256sum mismatch! Expected {expected_sha}, got {remote_sha}"
                        print(f"    Remote SHA-256: {remote_sha} (100% MATCH)")

                        # 5. Download fixture and verify downloaded SHA-256 (for sizes <= 1 MiB where max_chars <= 1,000,000)
                        if run_idx == 0 and size_bytes <= 1024 * 1024:
                            t0 = time.perf_counter()
                            dl_out = call_tool(proc, "ssh_download_file", {
                                "device_name": target_device,
                                "remote_path": remote_file,
                                "max_chars": 1000000
                            }, 4004 + idx * 100 + run_idx * 10)
                            dl_lat = (time.perf_counter() - t0) * 1000
                            dl_sec = dl_lat / 1000.0
                            dl_thru_kib = (size_kib / dl_sec) if dl_sec > 0 else 0
                            dl_thru_mb = (size_mb / dl_sec) if dl_sec > 0 else 0
                            thru_str_dl = f"{dl_thru_mb:.2f} MB/s" if size_bytes >= 1024 * 1024 else f"{dl_thru_kib:.1f} KiB/s"
                            print(f"    Download ({label}): {dl_lat:.2f}ms ({thru_str_dl})")

                            downloaded_bytes = None
                            try:
                                envelope = json.loads(dl_out)
                                if isinstance(envelope, dict) and "content_base64" in envelope:
                                    downloaded_bytes = base64.b64decode(envelope["content_base64"])
                            except Exception:
                                pass
                            if downloaded_bytes is None:
                                downloaded_bytes = dl_out.encode("utf-8")

                            actual_sha = hashlib.sha256(downloaded_bytes).hexdigest()
                            assert actual_sha == expected_sha, f"SHA-256 mismatch on download {label}! Got {actual_sha} vs {expected_sha}"
                            print(f"    Downloaded SHA-256 Integrity: 100% MATCH ({label})")
                    finally:
                        # 5. Clean up dedicated temporary directory
                        try:
                            call_tool_json(proc, "ssh_exec", {
                                "device_name": target_device,
                                "command": f"rm -rf '{remote_dir}'"
                            }, 4003 + idx * 100 + run_idx * 10)
                        except Exception as clean_err:
                            print(f"    Warning: cleanup of '{remote_dir}' failed: {clean_err}")

                if reps > 1:
                    med_sec = statistics.median(durations)
                    med_thru = statistics.median(throughputs)
                    thru_unit = "MB/s" if size_bytes >= 1024 * 1024 else "KiB/s"
                    print(f"    Median Upload ({label}): {med_sec:.2f}s ({med_thru:.2f} {thru_unit}) over {reps} runs")

            # Test long quoted path
            print("\n    --- Testing Long Quoted Path Transfer ---")
            quoted_dir = f"/tmp/mcp_test_quoted_{os.getpid()}_{int(time.time() * 1000)}"
            quoted_file = f"{quoted_dir}/path with spaces 'single quotes' and -dashes.txt"
            call_tool_json(proc, "ssh_exec", {
                "device_name": target_device,
                "command": f"mkdir -p '{quoted_dir}'"
            }, 4500)
            try:
                q_content = "content for long quoted path verification"
                up_q = call_tool(proc, "ssh_upload_file", {
                    "device_name": target_device,
                    "remote_path": quoted_file,
                    "content": q_content
                }, 4501)
                assert "Successfully uploaded" in up_q
                dl_q = call_tool(proc, "ssh_download_file", {
                    "device_name": target_device,
                    "remote_path": quoted_file,
                    "max_chars": 10000
                }, 4502)
                assert q_content in dl_q
                print(f"    Long quoted path transfer verified: {quoted_file}")
            finally:
                call_tool_json(proc, "ssh_exec", {
                    "device_name": target_device,
                    "command": f"rm -rf '{quoted_dir}'"
                }, 4503)

            # Optional 10x 1 MiB Soak Test
            if args.repeat_1mb_soak:
                print("\n    --- Running 10x 1 MiB Upload Soak Test ---")
                mem_before = get_process_memory_mb(proc.pid)
                print(f"    Memory before soak: WS={mem_before[0]:.2f} MB, Priv={mem_before[1]:.2f} MB")
                soak_content = create_fixture(1024 * 1024)
                soak_dir = f"/tmp/mcp_test_soak_{os.getpid()}"
                call_tool_json(proc, "ssh_exec", {
                    "device_name": target_device,
                    "command": f"mkdir -p '{soak_dir}'"
                }, 4600)
                try:
                    for s_idx in range(10):
                        s_file = f"{soak_dir}/soak_{s_idx}.txt"
                        t0 = time.perf_counter()
                        up_s = call_tool(proc, "ssh_upload_file", {
                            "device_name": target_device,
                            "remote_path": s_file,
                            "content": soak_content
                        }, 4601 + s_idx)
                        lat_s = (time.perf_counter() - t0) * 1000
                        assert "Successfully uploaded" in up_s
                        print(f"      Soak run {s_idx+1}/10: {lat_s:.2f}ms")
                    mem_after = get_process_memory_mb(proc.pid)
                    print(f"    Memory after soak:  WS={mem_after[0]:.2f} MB, Priv={mem_after[1]:.2f} MB")
                    print(f"    Memory Delta: WS delta = {mem_after[0] - mem_before[0]:+.2f} MB, Priv delta = {mem_after[1] - mem_before[1]:+.2f} MB")
                finally:
                    call_tool_json(proc, "ssh_exec", {
                        "device_name": target_device,
                        "command": f"rm -rf '{soak_dir}'"
                    }, 4700)

        # 9. Memory Footprint Measurement
        ws_mb, priv_mb, peak_mb = get_process_memory_mb(proc.pid)
        print(f"\n[9] Memory Footprint: Working Set = {ws_mb:.2f} MB | Private Commit = {priv_mb:.2f} MB | Peak Working Set = {peak_mb:.2f} MB")
        if args.max_working_set_mb is not None:
            assert ws_mb <= args.max_working_set_mb, f"Working set {ws_mb:.2f} MB exceeds requested budget {args.max_working_set_mb:.2f} MB"

        # 10. Disconnect
        t0 = time.perf_counter()
        disc_out = call_tool(proc, "ssh_disconnect", {
            "device_name": target_device
        }, 5000)
        disc_lat = (time.perf_counter() - t0) * 1000
        print(f"[10] Disconnect: {disc_lat:.2f}ms | Result: {disc_out.strip()}")
        assert "Disconnected" in disc_out or "No active session" in disc_out

        print("\n>>> ALL BENCHMARK TESTS PASSED WITH 100% SUCCESS <<<")

    finally:
        try:
            proc.terminate()
            proc.wait(timeout=3)
        except Exception:
            pass

if __name__ == "__main__":
    main()
