"""Minimal Python SDK for the gvos tenant plane (Phase E first slice).
Read-only surface; generated clients replace this at Phase E step 2."""
import requests

class GvosClient:
    def __init__(self, base, key, timeout=15):
        self.base = base.rstrip("/")
        self.key = key
        self.s = requests.Session()
        self.s.headers["Authorization"] = f"Bearer {key}"
        self.s.timeout = timeout

    def _get(self, path):
        r = self.s.get(self.base + path)
        r.raise_for_status()
        return r.json()

    def me(self): return self._get("/v1/tenant/me")
    def accounts(self): return self._get("/v1/tenant/accounts")
    def cdrs(self, cursor=0): return self._get(f"/v1/tenant/cdrs?cursor={cursor}")
    def stats(self): return self._get("/v1/tenant/stats")
