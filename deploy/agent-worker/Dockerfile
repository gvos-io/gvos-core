# G-VOS voice-agent spike worker image.
#
# pipecat-ai PyPI releases (1.16.0 as of this writing) do not yet
# include the transports/sip module — it lives on GitHub main. Pin the
# commit so the image is reproducible; when a PyPI release ships
# `pipecat-ai[sip]`, swap this line for the versioned wheel.
#
# The sip extra pulls baresip-python (manylinux wheel, no native
# build) + opencv-python-headless.
FROM python:3.12-slim

WORKDIR /app

# git needed only for the pip git-install; removed afterwards to keep
# the runtime image lean.
RUN apt-get update -qq && apt-get install -y -qq --no-install-recommends git \
    && pip install --no-cache-dir \
       "pipecat-ai[sip] @ git+https://github.com/pipecat-ai/pipecat.git@834903d9a0ccc95038fb7890c7e35a47d680bcbd" \
    && apt-get purge -y -qq git && apt-get autoremove -y -qq && rm -rf /var/lib/apt/lists/*

COPY bot.py .

# SIP listen port (trunk mode: the edge dials in, we never REGISTER).
EXPOSE 5070/udp 5070/tcp

CMD ["python", "bot.py"]
