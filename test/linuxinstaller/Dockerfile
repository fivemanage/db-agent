FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends python3 openssl curl ca-certificates \
    && rm -rf /var/lib/apt/lists/*
# These are bootstrap logic fixtures, not a simulated claim of a live systemd test.
RUN mkdir -p /run/systemd/system \
    && touch /run/db-agent-installer-proof \
    && printf '#!/bin/sh\nexit 0\n' > /usr/bin/systemctl \
    && chmod 755 /usr/bin/systemctl
COPY install-linux.sh /usr/local/bin/install-linux.sh
COPY test_installer.py /test_installer.py
CMD ["python3", "/test_installer.py", "-v"]
