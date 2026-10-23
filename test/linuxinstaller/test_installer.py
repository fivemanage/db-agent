import base64
import hashlib
import http.server
import os
import pathlib
import platform
import shutil
import ssl
import subprocess
import tempfile
import threading
import unittest


INSTALLER = "/usr/local/bin/install-linux.sh"
EXECUTABLE = pathlib.Path("/opt/db-agent/db-agent")
EXECUTED = pathlib.Path("/tmp/agent-executed")
ARGUMENTS = pathlib.Path("/tmp/agent-arguments")


def openssl(*args):
    return subprocess.run(["openssl", *args], check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE).stdout


class InstallerTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        if (os.environ.get("DB_AGENT_INSTALLER_PROOF") != "1" or os.geteuid() != 0
                or not pathlib.Path("/run/db-agent-installer-proof").is_file()):
            raise RuntimeError("Run only with scripts/test-linux-installer.sh in its disposable container")
        cls.work = tempfile.TemporaryDirectory()
        cls.addClassCleanup(cls.work.cleanup)
        cls.key = os.path.join(cls.work.name, "signing.key")
        openssl("genpkey", "-algorithm", "Ed25519", "-out", cls.key)
        der = openssl("pkey", "-in", cls.key, "-pubout", "-outform", "DER")
        cls.public_key = base64.b64encode(der[-32:]).decode("ascii")
        certificate = os.path.join(cls.work.name, "certificate.pem")
        tls_key = os.path.join(cls.work.name, "tls.key")
        openssl("req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
                "-keyout", tls_key, "-out", certificate, "-subj", "/CN=localhost",
                "-addext", "subjectAltName=DNS:localhost,IP:127.0.0.1")

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                cls.requests.append(self.path)
                if self.path == "/SHA256SUMS":
                    body = cls.checksums
                elif self.path == "/SHA256SUMS.sig":
                    body = cls.signature
                elif self.path == "/" + cls.artifact_name:
                    if cls.insecure_redirect:
                        self.send_response(302)
                        self.send_header("Location", "http://127.0.0.1/artifact")
                        self.end_headers()
                        return
                    body = cls.download_bytes
                else:
                    self.send_error(404)
                    return
                self.send_response(200)
                if cls.content_length:
                    self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *_):
                pass

        cls.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(certificate, tls_key)
        cls.server.socket = context.wrap_socket(cls.server.socket, server_side=True)
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()
        cls.addClassCleanup(cls.server.server_close)
        cls.addClassCleanup(cls.server.shutdown)
        cls.url = "https://localhost:%d" % cls.server.server_port
        cls.environment = dict(os.environ, SSL_CERT_FILE=certificate,
                               CURL_CA_BUNDLE=certificate,
                               DB_AGENT_RELEASE_PUBLIC_KEY=cls.public_key,
                               DB_AGENT_RELEASE_BASE_URL=cls.url)

    def setUp(self):
        self.clean_installation()
        type(self).requests = []
        type(self).insecure_redirect = False
        type(self).content_length = True
        self.artifact = b"""#!/bin/sh
touch /tmp/agent-executed
if [ "$1" = version ]; then
  printf '1.2.3\\n'
elif [ "$1" = install ]; then
  printf '%s\\n' "$@" > /tmp/agent-arguments
  if [ "${FIXTURE_SETUP_EXIT:-0}" != 0 ]; then
    echo 'fixture setup failed; saved state preserved' >&2
    exit "$FIXTURE_SETUP_EXIT"
  fi
else
  exit 1
fi
"""
        arch = {"x86_64": "amd64", "aarch64": "arm64"}[platform.machine()]
        type(self).artifact_name = "db-agent-1.2.3-linux-" + arch
        self.inventory = "%s  %s\n" % (hashlib.sha256(self.artifact).hexdigest(), self.artifact_name)
        self.sign()
        os.mkdir("/etc/db-agent", 0o700)
        self.saved = {"config.yaml": b"agent_credential: local-test-credential\n",
                      "encryption.key": b"original-local-test-key\n"}
        for name, contents in self.saved.items():
            path = pathlib.Path("/etc/db-agent", name)
            path.write_bytes(contents)
            path.chmod(0o600)

    def tearDown(self):
        for name, contents in self.saved.items():
            self.assertEqual(pathlib.Path("/etc/db-agent", name).read_bytes(), contents)
        self.assertEqual(list(pathlib.Path("/opt/db-agent").glob(".install-*")), [])
        self.clean_installation()

    def clean_installation(self):
        for name in ("/opt/db-agent", "/etc/db-agent"):
            path = pathlib.Path(name)
            if path.is_symlink():
                path.unlink()
            elif path.exists():
                shutil.rmtree(path)
        EXECUTED.unlink(missing_ok=True)
        ARGUMENTS.unlink(missing_ok=True)

    def sign(self):
        payload = "fivemanage-db-agent-checksums-v1\n" + self.inventory
        payload_path = os.path.join(self.work.name, "payload")
        pathlib.Path(payload_path).write_text(payload)
        signature = openssl("pkeyutl", "-sign", "-inkey", self.key, "-rawin", "-in", payload_path)
        type(self).signature = base64.b64encode(signature) + b"\n"
        type(self).checksums = self.inventory.encode("utf-8")
        type(self).download_bytes = self.artifact

    def run_installer(self, *, environment=None, args=None, user=None, script=INSTALLER):
        return subprocess.run(["bash", script, *(args if args is not None else ["--service-only"])],
                              env=environment or self.environment, stdin=subprocess.DEVNULL,
                              stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=30,
                              preexec_fn=(lambda: os.setuid(user)) if user is not None else None)

    def assert_rejected(self, result, message, *, executed=False):
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(message, result.stderr.decode("utf-8"))
        self.assertFalse(EXECUTABLE.exists())
        self.assertEqual(EXECUTED.exists(), executed)

    def test_verified_install_uses_existing_wizard_and_preserves_private_files(self):
        result = self.run_installer(args=["--service-only", "--config", "/etc/db-agent/config.yaml"])
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(EXECUTABLE.read_bytes(), self.artifact)
        self.assertEqual(EXECUTABLE.stat().st_mode & 0o777, 0o755)
        self.assertEqual(pathlib.Path("/etc/db-agent").stat().st_mode & 0o777, 0o700)
        self.assertEqual(ARGUMENTS.read_text().splitlines(), ["install", "--service-only", "--config", "/etc/db-agent/config.yaml"])

    def test_install_does_not_require_python_on_the_customer_path(self):
        interpreter = pathlib.Path("/usr/bin/python3")
        hidden = pathlib.Path("/usr/bin/python3.disabled-for-installer-proof")
        interpreter.rename(hidden)
        try:
            result = self.run_installer()
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(EXECUTABLE.read_bytes(), self.artifact)
        finally:
            hidden.rename(interpreter)

    def test_bad_signature_does_not_fetch_or_execute_artifact(self):
        type(self).signature = base64.b64encode(bytes(64))
        self.assert_rejected(self.run_installer(), "signature is invalid")
        self.assertNotIn("/" + self.artifact_name, self.requests)

    def test_checksum_mismatch_does_not_execute_download(self):
        type(self).download_bytes = b"untrusted bytes"
        self.assert_rejected(self.run_installer(), "checksum mismatch")

    def test_signed_wrong_platform_is_rejected(self):
        self.inventory = self.inventory.replace("linux-", "windows-")
        self.sign()
        self.assert_rejected(self.run_installer(), "missing or ambiguous")

    def test_duplicate_platform_record_is_rejected(self):
        self.inventory += self.inventory
        self.sign()
        self.assert_rejected(self.run_installer(), "missing or ambiguous")

    def test_oversized_checksums_are_rejected(self):
        type(self).checksums = b"x" * (65536 + 1)
        self.assert_rejected(self.run_installer(), "size limits")

    def test_oversized_checksums_without_content_length_are_rejected(self):
        type(self).content_length = False
        type(self).checksums = b"x" * (65536 + 1)
        self.assert_rejected(self.run_installer(), "size limits")

    def test_http_redirect_is_rejected(self):
        type(self).insecure_redirect = True
        self.assert_rejected(self.run_installer(), "download failed")

    def test_missing_trust_fails_closed(self):
        self.assert_rejected(self.run_installer(environment=dict(self.environment, DB_AGENT_RELEASE_PUBLIC_KEY="")), "signing trust is not provisioned")
        self.assertEqual(self.requests, [])

    def test_untrusted_https_certificate_is_rejected(self):
        self.assert_rejected(self.run_installer(environment=dict(self.environment, CURL_CA_BUNDLE="/etc/ssl/certs/ca-certificates.crt")), "check HTTPS certificates")

    def test_existing_install_is_never_replaced(self):
        os.mkdir("/opt/db-agent", 0o755)
        EXECUTABLE.write_bytes(b"preserve existing binary")
        result = self.run_installer()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(b"installation already exists", result.stderr)
        self.assertEqual(EXECUTABLE.read_bytes(), b"preserve existing binary")
        self.assertFalse(EXECUTED.exists())
        self.assertEqual(self.requests, [])

    def test_symlink_install_directory_is_rejected(self):
        os.symlink(self.work.name, "/opt/db-agent")
        self.assert_rejected(self.run_installer(), "regular directories, not links")

    def test_unsafe_config_directory_is_not_chmodded_or_used(self):
        os.chmod("/etc/db-agent", 0o755)
        self.assert_rejected(self.run_installer(), "must be private")
        self.assertEqual(pathlib.Path("/etc/db-agent").stat().st_mode & 0o777, 0o755)

    def test_setup_failure_retains_verified_binary_and_error_status(self):
        result = self.run_installer(environment=dict(self.environment, FIXTURE_SETUP_EXIT="9"))
        self.assertEqual(result.returncode, 9)
        self.assertIn(b"fixture setup failed", result.stderr)
        self.assertEqual(EXECUTABLE.read_bytes(), self.artifact)

    def test_full_setup_requires_a_real_terminal(self):
        self.assert_rejected(self.run_installer(args=[]), "unredirected terminal")
        self.assertEqual(self.requests, [])

    def test_non_root_is_rejected_before_network_or_writes(self):
        self.assert_rejected(self.run_installer(user=65534), "requires root")
        self.assertEqual(self.requests, [])

    def test_unsupported_architecture_is_rejected(self):
        replacement = pathlib.Path("/usr/local/bin/unsupported-installer.sh")
        replacement.write_text(pathlib.Path(INSTALLER).read_text().replace('"$(uname -m)"', '"s390x"'))
        try:
            self.assert_rejected(self.run_installer(script=str(replacement)), "Unsupported Linux architecture")
        finally:
            replacement.unlink()

    def test_published_installer_ignores_environment_trust_overrides(self):
        replacement = pathlib.Path("/usr/local/bin/published-installer.sh")
        text = pathlib.Path(INSTALLER).read_text()
        text = text.replace("release_public_key=''", "release_public_key='%s'" % self.public_key)
        text = text.replace("release_base_url=''", "release_base_url='%s'" % self.url)
        replacement.write_text(text)
        try:
            result = self.run_installer(script=str(replacement), environment=dict(self.environment,
                                      DB_AGENT_RELEASE_PUBLIC_KEY="not-a-key", DB_AGENT_RELEASE_BASE_URL="http://untrusted.invalid"))
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(EXECUTABLE.read_bytes(), self.artifact)
        finally:
            replacement.unlink()


if __name__ == "__main__":
    unittest.main()
