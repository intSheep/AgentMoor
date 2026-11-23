"""Install the selected, checksum-pinned CLI into the disposable Linux test image."""
from contextlib import contextmanager
from pathlib import Path
import base64
import hashlib
import platform
import shutil
import sys
import tarfile
import tempfile
import urllib.request


@contextmanager
def verified_package(url, digest):
    with tempfile.TemporaryFile() as archive, urllib.request.urlopen(url, timeout=120) as response:
        shutil.copyfileobj(response, archive)
        archive.seek(0)
        if hashlib.file_digest(archive, "sha512").digest() != base64.b64decode(digest):
            raise ValueError("CLI package checksum mismatch")
        archive.seek(0)
        with tarfile.open(fileobj=archive, mode="r:gz") as tar:
            yield tar


def binary(url, digest, member, executable):
    dest = Path("/usr/local/bin") / executable
    with verified_package(url, digest) as tar, dest.open("wb") as out:
        shutil.copyfileobj(tar.extractfile(member), out)
    dest.chmod(0o755)


arch = {"aarch64": "arm64", "x86_64": "x64"}[platform.machine()]

agent = sys.argv[1]
if agent == "claude-code":
    name = "claude-code-linux-" + arch
    url = f"https://registry.npmjs.org/@anthropic-ai/{name}/-/{name}-2.1.330.tgz"
    member, exe = "package/claude", "claude"
    digests = {
        "arm64": "V4yanAY5EA5UpwwRPtjSfqDbNqAlUJeVn3NYZw2AgmRHjdpSsLtHAzbxRXfkAnbjwTXzE0uedjQxUY02ARPcsA==",
        "x64": "9Gn+V7DtJ8M1nztP9CeDVl9g3RtrepI7rOFTyPRRnZe8D6zebIHKmC7OvBauucuAP0X0k85SDN/LPVZyjsGezQ==",
    }
elif agent == "codex":
    url = f"https://registry.npmjs.org/@openai/codex/-/codex-0.171.0-alpha.13-linux-{arch}.tgz"
    member = f"package/vendor/{platform.machine()}-unknown-linux-musl/bin/codex"
    exe = "codex"
    digests = {
        "arm64": "SgY5yDT6bCSIOSX1cV1p4Exq27nRRd7XKa+aHUh5p1DypcV6ZEygUvin326OQmgyM1gGCchpAEjDz9V3wmMZCQ==",
        "x64": "XJzxIb7fne875B7AuQPOoBS0Fltmc1CqEudZld4+hRQgSjofmpWdNdsfbFhAdTP4m/00il7df5c42MqyOfGDwg==",
    }
elif agent == "opencode":
    name = "opencode-linux-" + arch + ("-baseline" if arch == "x64" else "")
    url = f"https://registry.npmjs.org/{name}/-/{name}-1.19.6.tgz"
    member, exe = "package/bin/opencode", "opencode"
    digests = {
        "arm64": "WnlZe6BgxKMeauniWrfiKNSq2tkz4FGwAdsBMeN5WPwsxURVu6Qm2eQuv1SvmJYGEmNKxyVfX3m1worvcP+c9Q==",
        "x64": "514i1cj8ZGcgQKXg3EqAe2YVV5ID7EK3u6LuIqsSPhpVnWDwWjiJejYzhOWHbRMYpfbVbt6DHabs9BUbwHn7zg==",
    }
elif agent == "gemini-cli":
    name = "node-linux-" + arch
    node_digest = {
        "arm64": "IZx0J52/1NDwAsY2iyWR9nlPIYuYviTWetjj8+ElvYQh2+XRhLfCfkbFEF/2O5vGI2hcUbyPNHV+nz342FbJXw==",
        "x64": "KHsob+Fv9YerATvYujMbiyJqnRCvLtrcr/eTNqRqiOSGp97EOiRP1yP47+knGgQ/xnc3XY8xzkW587dlZzNqRA==",
    }
    binary(f"https://registry.npmjs.org/{name}/-/{name}-24.22.3.tgz", node_digest[arch], "package/bin/node", "node")
    with verified_package("https://registry.npmjs.org/@google/gemini-cli/-/gemini-cli-0.70.0.tgz",
                          "5gilVH8BI2bjSCf+X6zgGpT75bLWzK9UoZS1akjaaBwgWhm1durzKkNC1M3FamcW40YoqV0PDi4hRCIsY3f9Sg==") as tar:
        tar.extractall("/opt/gemini", filter="data")
    script = Path("/opt/gemini/package/bundle/gemini.js")
    script.chmod(0o755)
    Path("/usr/local/bin/gemini").symlink_to(script)
    sys.exit(0)
else:
    raise ValueError(agent)

binary(url, digests[arch], member, exe)
