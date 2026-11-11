#!/usr/bin/env python3
"""Drive a local rust-analyzer over LSP with the settings printed by
`offshore editor --mode lsp-remote` and check that the derive macro of
the fixture is expanded: the type of `_len = Greeter::NAME.len()` is only
known if the proc-macro library (built remotely, read through the output
FUSE) was loaded.

usage: ra_lsp.py RUST_ANALYZER PROJECT CHECK_COMMAND_JSON
Prints the hover text and exits 0 when it shows `usize`.

rust-analyzer answers hovers only between its loading phases (workspace,
build scripts through the proxy, proc macros), which takes minutes on a
loaded host. The script therefore asks after each report of a quiescent
server (experimental/serverStatus) and tolerates slow answers, within an
overall deadline ($E2E_RA_TIMEOUT seconds, default 600).
"""
import json, os, signal, subprocess, sys, threading, time

ra, proj, check = sys.argv[1], sys.argv[2], json.loads(sys.argv[3])
# Own process group: the checks rust-analyzer spawns (offshore run cargo
# check, i.e. remote sessions) must end with it, or they keep a build slot
# leased while the next steps of the e2e run.
p = subprocess.Popen([ra], cwd=proj, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                     stderr=open(os.path.join(proj, "..", "ra-lsp.log"), "wb"), start_new_session=True)
lock = threading.Lock()
responses, nid = {}, [0]
status = {"quiescent": False, "seen": 0, "message": ""}  # last experimental/serverStatus
changed = threading.Condition(lock)

def send(msg):
    b = json.dumps(msg).encode()
    p.stdin.write(b"Content-Length: %d\r\n\r\n" % len(b) + b)
    p.stdin.flush()

def reader():
    while True:
        hdr = {}
        while True:
            line = p.stdout.readline()
            if not line:
                return
            line = line.strip()
            if not line:
                break
            k, v = line.decode().split(":", 1)
            hdr[k.lower()] = v.strip()
        msg = json.loads(p.stdout.read(int(hdr["content-length"])))
        if "id" in msg and "method" in msg:  # server request: accept
            send({"jsonrpc": "2.0", "id": msg["id"], "result": None})
        elif "id" in msg:
            with lock:
                responses[msg["id"]] = msg
                changed.notify_all()
        elif msg.get("method") == "experimental/serverStatus":
            with lock:
                prm = msg.get("params") or {}
                status["quiescent"] = bool(prm.get("quiescent"))
                status["message"] = prm.get("message") or ""
                status["seen"] += 1
                changed.notify_all()

threading.Thread(target=reader, daemon=True).start()

def request(method, params, timeout=60):
    with lock:
        nid[0] += 1
        i = nid[0]
    send({"jsonrpc": "2.0", "id": i, "method": method, "params": params})
    end = time.time() + timeout
    with lock:
        while i not in responses:
            left = end - time.time()
            if left <= 0:
                raise TimeoutError(method)
            changed.wait(left)
        return responses.pop(i)

def wait_quiescent(after, until):
    """Waits for a quiescent status reported after the count after."""
    with lock:
        while not (status["quiescent"] and status["seen"] > after):
            left = until - time.time()
            if left <= 0:
                return status["seen"]
            changed.wait(left)
        return status["seen"]

uri = "file://" + os.path.join(proj, "src", "main.rs")
text = open(os.path.join(proj, "src", "main.rs")).read()
opts = {
    "check": {"overrideCommand": check},
    "cargo": {"buildScripts": {"overrideCommand": check}},
    "procMacro": {"enable": True},
}
deadline = time.time() + float(os.environ.get("E2E_RA_TIMEOUT", "600"))
request("initialize", {"processId": os.getpid(), "rootUri": "file://" + proj,
                       "capabilities": {"experimental": {"serverStatusNotification": True}},
                       "initializationOptions": opts}, max(1, deadline - time.time()))
send({"jsonrpc": "2.0", "method": "initialized", "params": {}})
send({"jsonrpc": "2.0", "method": "textDocument/didOpen", "params": {
    "textDocument": {"uri": uri, "languageId": "rust", "version": 1, "text": text}}})
line = next(i for i, l in enumerate(text.splitlines()) if "_len" in l)
col = text.splitlines()[line].index("_len") + 1
hover, seen = "", 0
while time.time() < deadline:
    # Ask once the server is idle; also every 20s in case a status
    # notification is missed.
    seen = wait_quiescent(seen, min(deadline, time.time() + 20))
    try:
        r = request("textDocument/hover", {"textDocument": {"uri": uri}, "position": {"line": line, "character": col}},
                    max(1, deadline - time.time()))
    except TimeoutError:
        break
    res = r.get("result") or {}
    hover = json.dumps(res.get("contents", ""))
    if "usize" in hover:
        break
    time.sleep(1)
if "usize" not in hover:
    print("last server status:", json.dumps(status), file=sys.stderr)
print(hover)
try:
    request("shutdown", None, 10)
    send({"jsonrpc": "2.0", "method": "exit"})
    p.wait(10)
except Exception:
    pass
for sig in (signal.SIGTERM, signal.SIGKILL):
    try:
        os.killpg(p.pid, sig)  # rust-analyzer's children too
    except ProcessLookupError:
        break
    time.sleep(2)
p.wait()
sys.exit(0 if "usize" in hover else 1)
