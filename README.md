# rpi-umpx-decoder
This repository contains the MicroMPX set-up for [ZuidWest FM](https://www.zuidwestfm.nl/) in the Netherlands. It uses a Rapsberry Pi 4 and a [HiFiBerry DAC2 Pro XLR
](https://www.hifiberry.com/shop/boards/hifiberry-dac2-pro-xlr/) as audio output. It downloads the most recent version of the MicroMPX decoder from Thimeo, which is managed by systemd as service.

# How to prepare the Rapsberry Pi
- Install Raspberry Pi OS Lite 11 (bullseye) 64-bit
- Ensure you are root by running `sudo su`
- Download and run the install script with the command `/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/oszuidwest/rpi-umpx-decoder/main/pisetup.sh)"`

# How to configure MicroMPX
- If you want monitoring via UptimeRobot, add the contents of `allowlist.txt` to the MicroMPX configuration file

[WIP]
