# G-VOS voice-agent spike worker (pipecat-integration-plan.md §10 Spike).
#
# One SIP endpoint per process (trunk mode: no REGISTER). Answers the
# INVITE, holds the call open until BYE — deliberately no LLM/ASR/TTS:
# the spike verifies the G-VOS side only (BILL_LOCK, answer event, CDR,
# teardown). The pipeline slot list is where Phase 1 will insert
# VAD/ASR/LLM/TTS.
#
# Baresip accounts are answermode=manual; auto_answer lives in the
# pipecat transport and only acts while a PipelineTask with the
# transport's input/output is RUNNING. So the bot runs a real (empty)
# pipeline per call: incoming event -> build task -> run to completion.
#
# Env:
#   AGENT_PORT        SIP listen port        (default 5070)
#   AGENT_MAX_CALLS   max_concurrent_calls   (default 4)
#   AGENT_DOMAIN      SIP AOR domain          (default agent-spike)
#   AGENT_TRACE       sip_trace=1 for debug
import asyncio
import os

from loguru import logger

from pipecat.pipeline.pipeline import Pipeline
from pipecat.pipeline.runner import PipelineRunner
from pipecat.pipeline.task import PipelineTask, PipelineParams
from pipecat.transports.sip import connection as sipconn_mod
from pipecat.transports.sip.connection import SIPConnection
from pipecat.transports.sip.transport import SIPParams, SIPTransport

PORT = int(os.getenv("AGENT_PORT", "5070"))
MAX_CALLS = int(os.getenv("AGENT_MAX_CALLS", "4"))
TRACE = os.getenv("AGENT_TRACE", "") == "1"
# Inbound AOR: what the edge puts in the INVITE Request-URI. The demo
# caller dials 8613800138000 and Kamailio relays the URI unchanged, so
# the worker claims that (user, domain) pair. Real deployments set
# AOR_DOMAIN to the edge's advertised host and route an agent number
# family here.
AOR_USER = os.getenv("AGENT_AOR_USER", "8613800138000")
AOR_DOMAIN = os.getenv("AGENT_AOR_DOMAIN", "172.22.0.8:5060")


def _primary_ip() -> str:
    # The address baresip should advertise in Via/Contact/SDP: the
    # container's primary IP. UDP-connect trick needs no traffic.
    import socket
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    try:
        s.connect(("8.8.8.8", 80))
        return s.getsockname()[0]
    finally:
        s.close()


IP = os.getenv("AGENT_IP") or _primary_ip()


# --- baresip listen binding ---------------------------------------
# Upstream gap (pipecat main 834903d): SIPConnection drops any
# extra_config_text passed by the caller — _RuntimeSettings.to_config()
# hardcodes the jitter-buffer lines, so there is no documented way to
# make the worker listen on a chosen SIP port. Workaround: wrap
# to_config so the sip_listen line (baresip native config syntax,
# space-separated; verified: binds 0.0.0.0:5070) rides along.
_orig_to_config = sipconn_mod._RuntimeSettings.to_config


def _to_config_with_listen(self):
    cfg = _orig_to_config(self)
    cfg.extra_config_text = (cfg.extra_config_text or "") + (
        f"\nsip_listen 0.0.0.0:{PORT}\n"
    )
    return cfg


sipconn_mod._RuntimeSettings.to_config = _to_config_with_listen


async def _run_call(transport: SIPTransport):
    # Empty pipeline (input -> output): no processing, but a RUNNING
    # task is what makes auto_answer answer the INVITE.
    pipeline = Pipeline([transport.input(), transport.output()])
    task = PipelineTask(pipeline, params=PipelineParams(allow_interruptions=True))
    runner = PipelineRunner()
    await runner.run(task)


class AgentWorker:
    def __init__(self):
        # Trunk mode: reg_interval=0 — the worker never REGISTERs, the
        # G-VOS edge (Kamailio GW_DISPATCH) dials this process directly.
        # Inbound matching: baresip routes an INVITE to the account
        # whose address-of-record matches the Request-URI. Kamailio
        # relays the INVITE with the original callee URI
        # (sip:<number>@<edge>), so the account must claim that domain
        # (AOR_USER is the agent number family the edge routes here,
        # e.g. "8613800138000" or an agent prefix).
        self.conn = SIPConnection(
            user=AOR_USER,
            domain=AOR_DOMAIN,
            password="",
            reg_interval=0,
            dtmf_mode="rtpevent",
            max_concurrent_calls=MAX_CALLS,
            sip_trace=TRACE,
            net_interface=IP,
        )
        self.transport = SIPTransport(
            self.conn,
            SIPParams(audio_in_enabled=True, audio_out_enabled=True),
        )
        # A one-connection-one-call model: the shared connection sees
        # the incoming call, we spawn a fresh transport per call in
        # _on_incoming (each SIPTransport/SIPConnection pair serves
        # exactly one call upstream).
        self.conn.event_handler("incoming")(self._on_incoming)
        self.conn.event_handler("call_closed")(self._on_closed)

    async def _on_incoming(self, call, data):
        logger.info(f"INCOMING call={call} data={data}")
        try:
            await _run_call(self.transport)
        except Exception as e:
            logger.error(f"call task ended: {e}")

    async def _on_closed(self, call, data):
        # BYE -> gvos.cdr.raw -> CDRR durable -> PG.
        logger.info(f"CLOSED call={call} data={data}")

    async def run(self):
        # connect() starts the shared baresip runtime (through the
        # patched to_config -> sip_listen) and binds the account.
        # Registration is skipped in trunk mode (reg_interval=0).
        await self.conn.connect()
        logger.info(f"agent worker up (trunk mode, port {PORT}, ip {IP})")
        while True:
            await asyncio.sleep(3600)


async def main():
    worker = AgentWorker()
    await worker.run()


if __name__ == "__main__":
    asyncio.run(main())
