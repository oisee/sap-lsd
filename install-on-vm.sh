#!/usr/bin/env bash
# Install sap-lsd as a systemd service. Run from a checkout that has been built
# (make build) or from a release bundle that contains bin/sap-lsd + assets/.
set -euo pipefail
sudo mkdir -p /opt/sap-lsd/assets
sudo cp bin/sap-lsd /opt/sap-lsd/
sudo cp assets/probe.scrubbed.jsonl assets/show.json /opt/sap-lsd/assets/
sudo chmod +x /opt/sap-lsd/sap-lsd
sudo cp sap-lsd.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now sap-lsd
sleep 1
sudo systemctl --no-pager status sap-lsd | head -6
sudo ss -ltnp | grep 3200 || echo "(not listening yet)"
