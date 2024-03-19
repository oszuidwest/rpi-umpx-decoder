# rpi-umpx-decoder
This repository contains the MicroMPX setup for [ZuidWest FM](https://www.zuidwestfm.nl/) in the Netherlands. It is compatible with Raspberry Pi 4 or 5 and uses a [HiFiBerry DAC2 Pro XLR](https://www.hifiberry.com/shop/boards/hifiberry-dac2-pro-xlr/) for audio output. The repository facilitates downloading the latest version of the MicroMPX decoder from Thimeo, which systemd manages as a service.

# How to Prepare the Raspberry Pi
- Install Raspberry Pi OS Lite 12 (Bookworm).
- Ensure you are root by executing `sudo su`.
- Download and execute the installation script with the command: `/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/oszuidwest/rpi-umpx-decoder/main/setup.sh)"`.
- After rebooting, MicroMPX should be accessible at `http://{{ip}}:8080`.

## A Few Words About the Raspberry Pi 5
MicroMPX and HiFiBerry boards are compatible with the Raspberry Pi 5. Currently, there is [a firmware bug](https://github.com/raspberrypi/linux/issues/5743) that necessitates manually editing the `/boot/firmware/config.txt` file. Add `,slave` to the `dtoverlay` line for the HiFiBerry, such as `dtoverlay=hifiberry-dacplus,slave`.

# How to Connect the Raspberry Pi to the VPN
- Download and execute the VPN script with the command: `/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/oszuidwest/rpi-umpx-decoder/main/vpn.sh)"`.
- Verify the presence of an interface named `wg0` with the correct IP using `ip a`.
- If the `wg0` interface does not appear, enable debugging with `modprobe wireguard && echo module wireguard +p > /sys/kernel/debug/dynamic_debug/control` and use `tail -f /var/log/syslog` to identify any errors.

## Optional Heartbeat Monitoring
Optionally, heartbeat monitoring can be integrated. In this configuration, the Pi will execute `wget --spider` on a specified URL every minute to serve as a heartbeat. This can be any URL, but testing was conducted with [Uptime Robot](https://uptimerobot.com/?rid=6f699dbd539740). Note that a paid Uptime Robot account is required for heartbeat monitoring.

# Opinionated Modifications
This script introduces several modifications to the default MicroMPX behavior, which we believe enhance the setup:

1. Logs are automatically deleted after 7 days instead of the default 30 days in MicroMPX. A cron job is set up to manage this.
2. To preserve the SD card from excessive writes, logs are written to a RAM disk instead of the SD card. MicroMPX logs can be quite verbose, and without this modification, we have experienced failures with some SD cards.
3. The HDMI audio output is disabled, allowing the analog output to be used for signal monitoring. Therefore, we recommend using a Raspberry Pi 4, as it still offers an analog output.

# License
This project is licensed under the GPLv3 License - see the LICENSE file for details.

For bugs, feedback, and ideas, please contact us at `techniek@zuidwesttv.nl` or file a pull request with your idea.
