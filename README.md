# rpi-umpx-decoder
This repository contains the MicroMPX set-up for [ZuidWest FM](https://www.zuidwestfm.nl/) in the Netherlands. It uses a Rapsberry 3, 4 or 5 and a [HiFiBerry DAC2 Pro XLR
](https://www.hifiberry.com/shop/boards/hifiberry-dac2-pro-xlr/) as audio output. It downloads the most recent version of the MicroMPX decoder from Thimeo, which is managed by systemd as service.

# How to prepare the Rapsberry Pi
- Install Raspberry Pi OS Lite 12 (Bookworm) or 11 (Bullseye) 64-bit
- Ensure you are root by running `sudo su`
- Download and run the install script with the command `/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/oszuidwest/rpi-umpx-decoder/main/setup.sh)"`

# How to configure MicroMPX
- After a reboot MicroMPX should be running on `http://{{ip}}:8080`
- If you want monitoring via UptimeRobot, add the contents of `allowlist.txt` to the `Whitelist=` section of hte MicroMPX configuration file which is at `/home/micrompx/.MicroMPX_Decoder.rc`

# How to add the Raspberry Pi to the VPN
- Download and run the VPN script with the command `/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/oszuidwest/rpi-umpx-decoder/main/vpn.sh)"`
- Check with `ip a` if you have an interface named `wg0` with the correct IP
- If the `wg0` interface is not showing, enable debugging with `modprobe wireguard && echo module wireguard +p > /sys/kernel/debug/dynamic_debug/control` and `tail -f /var/log/syslog` to look for errors

## Optional heartbeat monitoring
You can optionally integrate heartbeat monitoring. In this case the Pi will `wget --spider` a given url every minute, acting as a heartbeat. This can be any url, but we tested with Uptime Robot. A paid account is required at Uptime Robot for heartbeat monitoring.
