# PiQuant Systemd Service Files

This directory contains the production systemd service definitions:

- `piquant-agent.service`: For Raspberry Pi 5 (authoritative research computer). Runs `piquant agent`.
- `piquant-dashboard.service`: For Raspberry Pi 3B (dashboard and lightweight worker). Runs `piquant dashboard`.

### Quick Installation:
```bash
# On Pi 5:
sudo make install ROLE=agent

# On Pi 3B:
sudo make install ROLE=dashboard
```
