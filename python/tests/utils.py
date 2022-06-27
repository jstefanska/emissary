import logging
import json
import os
import subprocess
import requests
import socket
import tempfile
import time
from collections import namedtuple
from retry import retry
from OpenSSL import crypto
from base64 import b64encode
from typing import List, Literal, cast

import json
import yaml

from ambassador import Cache, IR
from ambassador.compile import Compile
from ambassador.utils import NullSecretHandler

from tests.manifests import cleartext_host_manifest
from tests.kubeutils import apply_kube_artifacts
from tests.runutils import run_and_assert

logger = logging.getLogger("ambassador")

def zipkin_tracing_service_manifest():
    return """
---
apiVersion: getambassador.io/v3alpha1
kind: TracingService
metadata:
  name: tracing
  namespace: ambassador
spec:
  service: zipkin:9411
  driver: zipkin
  config: {}
"""

def default_listener_manifests():
    return """
---
apiVersion: getambassador.io/v3alpha1
kind: Listener
metadata:
  name: listener-8080
  namespace: default
spec:
  port: 8080
  protocol: HTTPS
  securityModel: XFP
  hostBinding:
    namespace:
      from: ALL
---
apiVersion: getambassador.io/v3alpha1
kind: Listener
metadata:
  name: listener-8443
  namespace: default
spec:
  port: 8443
  protocol: HTTPS
  securityModel: XFP
  hostBinding:
    namespace:
      from: ALL
"""

def default_http3_listener_manifest():
  return """
---
apiVersion: getambassador.io/v3alpha1
kind: Listener
metadata:
  name: listener-http3-8443
  namespace: default
spec:
  port: 8443
  protocolStack:
    - TLS
    - HTTP
    - UDP
  securityModel: XFP
  hostBinding:
    namespace:
      from: ALL
  """

def default_udp_listener_manifest():
  return """
---
apiVersion: getambassador.io/v3alpha1
kind: Listener
metadata:
  name: listener-udp-8443
  namespace: default
spec:
  port: 8443
  protocolStack:
    - TLS
    - UDP
  securityModel: XFP
  hostBinding:
    namespace:
      from: ALL
  """
def default_tcp_listener_manifest():
  return """
---
apiVersion: getambassador.io/v3alpha1
kind: Listener
metadata:
  name: listener-tcp-8443
  namespace: default
spec:
  port: 8443
  protocolStack:
    - TLS
    - TCP
  securityModel: XFP
  hostBinding:
    namespace:
      from: ALL
  """

def _require_no_errors(ir: IR):
    assert ir.aconf.errors == {}

def _secret_handler():
    source_root = tempfile.TemporaryDirectory(prefix="null-secret-", suffix="-source")
    cache_dir = tempfile.TemporaryDirectory(prefix="null-secret-", suffix="-cache")
    return NullSecretHandler(logger, source_root.name, cache_dir.name, "fake")

def compile_with_cachecheck(yaml, errors_ok=False):
    # Compile with and without a cache. Neither should produce errors.
    cache = Cache(logger)
    secret_handler = _secret_handler()
    r1 = Compile(logger, yaml, k8s=True, secret_handler=secret_handler)
    r2 = Compile(logger, yaml, k8s=True, secret_handler=secret_handler, cache=cache)

    if not errors_ok:
        _require_no_errors(r1["ir"])
        _require_no_errors(r2["ir"])

    # Both should produce equal Envoy config as sorted json.
    r1j = json.dumps(r1['xds'].as_dict(), sort_keys=True, indent=2)
    r2j = json.dumps(r2['xds'].as_dict(), sort_keys=True, indent=2)
    assert r1j == r2j

    # All good.
    return r1

EnvoyFilterInfo = namedtuple('EnvoyFilterInfo', [ 'name', 'type' ])

EnvoyHCMInfo = EnvoyFilterInfo(
    name="envoy.filters.network.http_connection_manager",
    type="type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager"
)

EnvoyTCPInfo = EnvoyFilterInfo(
    name="envoy.filters.network.tcp_proxy",
    type="type.googleapis.com/envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy"
)

def econf_compile(yaml):
    compiled = compile_with_cachecheck(yaml)
    return compiled['xds'].as_dict()

def assert_valid_envoy_config(config_dict, extra_dirs=[]):
    with tempfile.TemporaryDirectory() as tmpdir:
        econf = open(os.path.join(tmpdir, 'econf.json'), 'xt')
        econf.write(json.dumps(config_dict))
        econf.close()
        img = os.environ.get('ENVOY_DOCKER_TAG')
        assert img
        cmd = [
            'docker', 'run',
            '--rm',
            f"--volume={tmpdir}:/ambassador:ro",
            *[f"--volume={extra_dir}:{extra_dir}:ro" for extra_dir in extra_dirs],
            img,
            '/usr/local/bin/envoy-static-stripped',
            '--config-path', '/ambassador/econf.json',
            '--mode', 'validate',
        ]
        p = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        if p.returncode != 0:
            print(p.stdout.decode())
        p.check_returncode()


def create_crl_pem_b64(issuerCert, issuerKey, revokedCerts):
    when = b"20220516010101Z"
    crl = crypto.CRL()
    crl.set_lastUpdate(when)

    for revokedCert in revokedCerts:
        clientCert = crypto.load_certificate(crypto.FILETYPE_PEM, bytes(revokedCert, "utf-8"))
        r = crypto.Revoked()
        r.set_serial(bytes('{:x}'.format(clientCert.get_serial_number()), "ascii"))
        r.set_rev_date(when)
        r.set_reason(None)
        crl.add_revoked(r)

    cert = crypto.load_certificate(crypto.FILETYPE_PEM, bytes(issuerCert, "utf-8"))
    key = crypto.load_privatekey(crypto.FILETYPE_PEM, bytes(issuerKey, "utf-8"))
    crl.sign(cert, key, b"sha256")
    return b64encode((crypto.dump_crl(crypto.FILETYPE_PEM, crl).decode("utf-8")+"\n").encode('utf-8')).decode('utf-8')
