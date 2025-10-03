# rpi-umpx-decoder
This repository contains the MicroMPX decoder software for [ZuidWest FM](https://www.zuidwestfm.nl/) and [BredaNu](https://www.bredanu.nl/) in the Netherlands. The setup involves a Raspberry Pi 4 or 5 and a HiFiBerry audio board (DAC, DAC+, DAC2, Digi, etc.) for audio output. The system uses the [Thimeo MicroMPX decoder](https://www.thimeo.com/stereo-tool/micrompx-decoder/), which decodes MicroMPX composite signals for FM radio monitoring and distribution.

# Preparing the Raspberry Pi
- Install Raspberry Pi OS Lite 12 (Bookworm) 64-bit.
- Follow the guide at https://www.hifiberry.com/docs/software/configuring-linux-3-18-x/ for HiFiBerry setup.
- Gain root access with `sudo su`.
- Download and execute the install script using `/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/oszuidwest/rpi-umpx-decoder/main/install.sh)"`.

# Configuration options during installation
The installer will prompt for:
- Whether to perform all OS updates (default: yes)
- Optional heartbeat monitoring integration with UptimeRobot
- Log retention period in days (default: 7)

After installation completes, the Pi will reboot automatically. The MicroMPX web interface will be accessible at `http://[raspberry-pi-ip]:8080`.

# HiFiBerry configuration examples
Add the appropriate device tree overlay for your HiFiBerry model to `/boot/firmware/config.txt`:
```bash
# For DAC+ Standard/Pro/XLR
dtoverlay=hifiberry-dacplus

# For DAC2 HD
dtoverlay=hifiberry-dacplushd

# For Digi+/Digi2
dtoverlay=hifiberry-digi
```
See HiFiBerry documentation for other models.

# Service management
Manage the MicroMPX service using standard systemd commands:
```bash
systemctl status micrompx     # Check service status
systemctl restart micrompx    # Restart service
journalctl -u micrompx -f     # View logs
systemctl status ramdisk      # Check RAM disk status
```

The MicroMPX binary is located at `/opt/micrompx/MicroMPX_Decoder`. Logs are written to `/home/micrompx/.MicroMPX_Decoder.log`, which is symlinked to a RAM disk at `/mnt/ramdisk` to protect your SD card from excessive writes. The systemd service files are in `/etc/systemd/system/`.

# Technical details
To prevent SD card wear from verbose logging, all MicroMPX logs are written to a 256MB RAM disk. Logs are automatically cleaned up after your configured retention period via a cron job. On Raspberry Pi 4, HDMI audio output is disabled to allow the analog output to be used for signal monitoring (note that Raspberry Pi 5 does not have an analog audio output).

If you enable heartbeat monitoring during installation, the system will perform a `wget --spider` request to your specified URL every minute, which works with uptime monitoring services like [Uptime Robot](https://uptimerobot.com/?rid=6f699dbd539740).

# Troubleshooting
If the script exits with "No HiFiBerry card configured", make sure you've added the correct `dtoverlay` line to your `/boot/firmware/config.txt` file and rebooted.

If the service won't start, check `journalctl -u micrompx -f` for error messages. Common issues include port 8080 already being in use or the audio device not being properly configured.

If you can't access the web interface, verify the service is running with `systemctl status micrompx` and make sure you're using the correct IP address.

# License
This project is licensed under the GPLv3 License - see the LICENSE file for details.

For bugs, feedback, and ideas, please contact us at `techniek@zuidwesttv.nl` or file a pull request with your idea.
