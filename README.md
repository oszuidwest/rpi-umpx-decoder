# rpi-umpx-decoder
This repository contains an automated MicroMPX decoder setup for [ZuidWest FM](https://www.zuidwestfm.nl/) in the Netherlands. It's designed for Raspberry Pi 4 or 5 with HiFiBerry audio boards and downloads the latest MicroMPX decoder from Thimeo, managing it as a systemd service.

## Prerequisites

### Hardware Requirements
- Raspberry Pi 4 or 5
- HiFiBerry audio board (DAC, DAC+, DAC2, Digi, etc.)
- SD card (minimum 8GB recommended)
- Stable internet connection

### Software Requirements
- Raspberry Pi OS Lite 12 (Bookworm)
- Pre-configured HiFiBerry device tree overlay

## Installation

### Step 1: Configure HiFiBerry
Before running the installation script, you must configure your HiFiBerry audio board in `/boot/firmware/config.txt` (or `/boot/config.txt` on older systems).

Add the appropriate device tree overlay for your HiFiBerry model:
```
# For DAC+ Standard/Pro/XLR
dtoverlay=hifiberry-dacplus

# For DAC2 HD
dtoverlay=hifiberry-dacplushd

# For Digi+/Digi2
dtoverlay=hifiberry-digi

# See HiFiBerry documentation for other models
```

### Step 2: Run Installation Script
1. Ensure you are root: `sudo su`
2. Execute the installation script:
```bash
/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/oszuidwest/rpi-umpx-decoder/main/setup.sh)"
```

### Step 3: Configuration Options
During installation, you'll be prompted for:
- **Heartbeat Monitoring**: Optional integration with UptimeRobot (requires paid account)
- **Log Retention**: Number of days to keep logs (default: 7 days)

### Step 4: Access MicroMPX
After the automatic reboot, MicroMPX will be accessible at:
- Web Interface: `http://{{raspberry-pi-ip}}:8080`

## Service Management

### Commands
```bash
# Check service status
systemctl status micrompx

# Start/stop/restart service
systemctl start micrompx
systemctl stop micrompx
systemctl restart micrompx

# View logs
journalctl -u micrompx -f

# Check RAM disk status
systemctl status ramdisk
```

### File Locations
- MicroMPX binary: `/opt/micrompx/MicroMPX_Decoder`
- Log directory: `/home/micrompx/.MicroMPX_Decoder.log` (symlinked to RAM disk)
- RAM disk mount: `/mnt/ramdisk`
- Service files: `/etc/systemd/system/micrompx.service`

## Features

### RAM Disk for Logs
To protect the SD card from excessive writes, all MicroMPX logs are written to a RAM disk (256MB). This prevents SD card failures from verbose logging.

### Automatic Log Cleanup
Logs are automatically deleted based on your configured retention period (default: 7 days) via a cron job, preventing the RAM disk from filling up.

### HDMI Audio Disabled
HDMI audio output is disabled to allow the analog output on Raspberry Pi 4 to be used for signal monitoring. Note that Raspberry Pi 5 does not have an analog audio output.

### Heartbeat Monitoring (Optional)
When enabled, the system performs a `wget --spider` request to your specified URL every minute. This is useful for uptime monitoring services like [Uptime Robot](https://uptimerobot.com/?rid=6f699dbd539740).

## Troubleshooting

### No HiFiBerry Detected
If the script exits with "No HiFiBerry card configured", ensure you've added the correct `dtoverlay` line to your config.txt file and rebooted.

### Service Won't Start
Check the logs with `journalctl -u micrompx -f` for error messages. Common issues:
- Port 8080 already in use
- Audio device not properly configured
- Insufficient permissions

### Web Interface Not Accessible
- Verify the service is running: `systemctl status micrompx`
- Check firewall settings
- Ensure you're using the correct IP address

# License
This project is licensed under the GPLv3 License - see the LICENSE file for details.

For bugs, feedback, and ideas, please contact us at `techniek@zuidwesttv.nl` or file a pull request with your idea.
