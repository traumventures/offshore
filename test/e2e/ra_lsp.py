#!/usr/bin/env python3
"""Drive a local rust-analyzer over LSP with the settings printed by
`cloudsourced editor --mode lsp-remote` and check that the derive macro of
the fixture is expanded: the type of `_len = Greeter::NAME.len()` is only
known if the proc-macro library (built remotely, read through the output
FUSE) was loaded.

usage: ra_lsp.py RUST_ANALYZER PROJECT CHECK_COMMAND_JSON
Prints the hover text and exits 0 when it shows `usize`.
"""
import json, os, subprocess, sys, threading, time

ra, proj, check = sys.argv[1], sys.argv[2], json.loads(sys.argv[3])
p = subprocess.Popen([ra], cwd=proj, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                     stderr=open(os.path.join(proj, "..", "ra-lsp.log"), "wb"))
lock = threading.Lock()
responses, nid = {}, [0]

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

threading.Thread(target=reader, daemon=True).start()

def request(method, params, timeout=60):
    with lock:
        nid[0] += 1
        i = nid[0]
    send({"jsonrpc": "2.0", "id": i, "method": method, "params": params})
    end = time.time() + timeout
    while time.time() < end:
        with lock:
            if i in responses:
                return responses.pop(i)
        time.sleep(0.05)
    raise TimeoutError(method)

uri = "file://" + os.path.join(proj, "src", "main.rs")
text = open(os.path.join(proj, "src", "main.rs")).read()
opts = {
    "check": {"overrideCommand": check},
    "cargo": {"buildScripts": {"overrideCommand": check}},
    "procMacro": {"enable": True},
}
request("initialize", {"processId": os.getpid(), "rootUri": "file://" + proj,
                       "capabilities": {}, "initializationOptions": opts}, 120)
send({"jsonrpc": "2.0", "method": "initialized", "params": {}})
send({"jsonrpc": "2.0", "method": "textDocument/didOpen", "params": {
    "textDocument": {"uri": uri, "languageId": "rust", "version": 1, "text": text}}})
line = next(i for i, l in enumerate(text.splitlines()) if "_len" in l)
col = text.splitlines()[line].index("_len") + 1
hover = ""
deadline = time.time() + 240
while time.time() < deadline:
    r = request("textDocument/hover", {"textDocument": {"uri": uri}, "position": {"line": line, "character": col}})
    res = r.get("result") or {}
    hover = json.dumps(res.get("contents", ""))
    if "usize" in hover:
        break
    time.sleep(2)
print(hover)
try:
    request("shutdown", None, 10)
    send({"jsonrpc": "2.0", "method": "exit"})
except Exception:
    pass
p.kill()
sys.exit(0 if "usize" in hover else 1)
