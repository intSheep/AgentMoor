"""Synthetic network/resource workload. No AgentMoor, model key or vendor CLI."""
import argparse
import errno
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import resource
import signal
import socket
import ssl
import struct
import threading
import time


def emit(value):
    print(json.dumps(value, sort_keys=True), flush=True)


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_GET(self):
        raw = json.dumps({"fixture": self.server.fixture, "port": self.server.server_port,
                          "process_id": os.getpid()}).encode()
        self.send_response(200)
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)


def serve(options):
    servers = []
    for port in options.ports:
        server = ThreadingHTTPServer(("0.0.0.0", port), Handler)
        server.fixture = options.nonce
        if port == 7443:
            context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
            context.load_cert_chain("/run/tls/tls.crt", "/run/tls/tls.key")
            server.socket = context.wrap_socket(server.socket, server_side=True)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        servers.append(server)
    emit({"server_ports": options.ports, "process_id": os.getpid(), "fixture": options.nonce})
    signal.pause()


def connect(target):
    started = time.monotonic()
    result = {"name": target["name"], "ip": target["ip"], "port": target["port"], "protocol": target.get("protocol", "TCP")}
    try:
        if result["protocol"] == "UDP":
            transaction = b"\x12\x34"
            query = transaction + b"\x01\x00\x00\x01\x00\x00\x00\x00\x00\x00" + b"\x0akubernetes\x07default\x03svc\x07cluster\x05local\0\x00\x01\x00\x01"
            with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as connection:
                connection.settimeout(.8)
                connection.sendto(query, (target["ip"], target["port"]))
                reply, _ = connection.recvfrom(4096)
                result["reachable"] = len(reply) >= 12 and reply[:2] == transaction
        else:
            with socket.create_connection((target["ip"], target["port"]), timeout=.8) as connection:
                result["reachable"] = True
                if target.get("tls"):
                    context = ssl.create_default_context(cafile="/opt/fixture-ca.pem")
                    connection = context.wrap_socket(connection, server_hostname="gateway.fixture")
                    result["tls_verified"] = True
                if target.get("tls") or target.get("http"):
                    connection.sendall(b"GET /probe HTTP/1.1\r\nHost: gateway.fixture\r\nConnection: close\r\n\r\n")
                    response = bytearray()
                    while chunk := connection.recv(4096):
                        response.extend(chunk)
                    head, body = bytes(response).split(b"\r\n\r\n", 1)
                    if not head.startswith(b"HTTP/1.0 200 "):
                        raise OSError("fixture HTTP did not return 200")
                    result["response"] = json.loads(body)
        result["error"] = None
    except (OSError, ssl.SSLError, ValueError) as error:
        result.update(reachable=False, error=type(error).__name__, errno=getattr(error, "errno", None),
                      timeout=isinstance(error, TimeoutError))
    result["seconds"] = time.monotonic() - started
    return result


def network(options):
    targets = json.loads(options.targets)
    emit({"targets": [connect(target) for target in targets],
          "uid": os.getuid(), "gid": os.getgid(), "hostname": socket.gethostname()})


def workload(options):
    while not Path("/run/work/start").exists():
        time.sleep(.02)
    virtual = {}
    for name in ("memory.max", "memory.swap.max", "memory.swap.current", "pids.max"):
        path = Path("/sys/fs/cgroup") / name
        try:
            virtual[name] = path.read_text().strip()
        except OSError as error:
            virtual[name] = {"unavailable_errno": error.errno}
    emit({"stage": "started", "virtual_cgroup": virtual, "mode": options.mode, "monotonic": time.monotonic(), "uid": os.getuid()})
    result = {"stage": "finished", "mode": options.mode}
    if options.mode == "cpu":
        stop = time.monotonic() + 6
        value = 0
        while time.monotonic() < stop:
            value = (value * 17 + 3) % 1000003
        result["iterations_value"] = value
    elif options.mode == "memory":
        pages = []
        for _ in range(256):
            pages.append(bytearray(b"m" * (1 << 20)))
            time.sleep(.005)
        raise RuntimeError("synthetic allocation unexpectedly exceeded the hard memory budget")
    elif options.mode in ("tmpfs", "file-memory"):
        directory = "/dev/shm" if options.mode == "tmpfs" else "/tmp"
        stat = os.statvfs(directory)
        result["filesystem_capacity_bytes"] = stat.f_blocks * stat.f_frsize
        limit = 96 if options.mode == "tmpfs" else 256
        written = 0
        try:
            with open(directory + "/bounded-fixture", "wb", buffering=0) as output:
                for _ in range(limit):
                    written += output.write(b"f" * (1 << 20))
        except OSError as error:
            result["write_errno"] = error.errno
        result["written_bytes"] = written
        if options.mode == "tmpfs" and result.get("write_errno") != errno.ENOSPC:
            raise RuntimeError("tmpfs workload did not observe its actual hard filesystem bound")
        if options.mode == "file-memory":
            raise RuntimeError("tmpfs write unexpectedly escaped the combined memory limit")
    elif options.mode == "pids":
        # The Go PID1 launcher installed the production hard limit before
        # exec. Check inheritance and denial before any bounded fork loop.
        result["guest_rlimit_nproc"] = resource.getrlimit(resource.RLIMIT_NPROC)
        try:
            resource.setrlimit(resource.RLIMIT_NPROC, (65, 65))
            result["hard_raise_denied"] = False
        except (OSError, ValueError):
            result["hard_raise_denied"] = True
        children = []
        try:
            for _ in range(128):
                try:
                    pid = os.fork()
                except OSError as error:
                    result["fork_errno"] = error.errno
                    break
                if pid == 0:
                    time.sleep(20)
                    os._exit(0)
                children.append(pid)
            result["children"] = len(children)
        finally:
            for pid in children:
                try:
                    os.kill(pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
            for pid in children:
                os.waitpid(pid, 0)
        if result.get("fork_errno") != errno.EAGAIN or not result["hard_raise_denied"]:
            raise RuntimeError("guest process limit was not enforced")
    elif options.mode in ("wall", "cancel"):
        time.sleep(120)
        raise RuntimeError("wall/cancellation fixture survived its external bound")
    else:
        raise RuntimeError("unknown synthetic workload")
    emit(result)
    time.sleep(120)  # Preserve cgroup evidence until trusted fixture cleanup.


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    server = sub.add_parser("serve")
    server.add_argument("--ports", type=int, nargs="+", required=True)
    server.add_argument("--nonce", required=True)
    network_parser = sub.add_parser("network")
    network_parser.add_argument("--targets", required=True)
    work = sub.add_parser("workload")
    work.add_argument("--mode", required=True)
    options = parser.parse_args()
    {"serve": serve, "network": network, "workload": workload}[options.command](options)


if __name__ == "__main__":
    main()
