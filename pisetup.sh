#!/bin/bash

# Function that checks if this is a supported platform
function check_platform() {
  if ! grep "Raspberry Pi 4" /proc/device-tree/model &>/dev/null; then
    echo -e "\e[1;31;5m** NOT RUNNING ON A RASPBERRY PI 4 **\e[0m"
    read -rp $'\e[3m\e[33mThis script is only tested on a Raspberry Pi 4. Press Enter to continue anyway...\e[0m\n'
  fi
}

# Check if running as root
if [ "$(id -u)" -ne 0 ]; then
  echo "This script must be run as root. Please use 'sudo bash pisetup.sh' to run it."
  exit 1
fi

# Check if we are running on a Raspberry PI 4
check_platform

# Check if the micrompx service is running
if systemctl is-active --quiet micrompx; then
  echo "MicroMPX service is running. Stopping it now..."

  # Stop the micrompx service and check the exit code directly
  if systemctl stop micrompx; then
    echo "MicroMPX service stopped successfully. We can now upgrade or reinstall."
  else
    echo "Failed to stop the MicroMPX service. Please check the logs for more details."
    exit 1
  fi
else
  echo "MicroMPX service is not running. Assuming fresh install."
fi

# Expand the filesystem
raspi-config --expand-rootfs

# Set the timezone
ln -fs /usr/share/zoneinfo/Europe/Amsterdam /etc/localtime
dpkg-reconfigure -f noninteractive tzdata

# Update all packages
apt -qq -y update
apt -qq -y upgrade
apt -qq -y autoremove

# Add the user micrompx if it doesn't exist
if ! id -u micrompx > /dev/null 2>&1; then
  useradd -m micrompx --home /home/micrompx --shell /usr/sbin/nologin --comment "micrompx daemon user"
fi

# Install dependencies
apt -qq -y install libasound2

# Download and install MicroMPX
mkdir -p /opt/micrompx
wget https://www.stereotool.com/download/MicroMPX_Decoder_ARM64_BETA1002-066 -O /opt/micrompx/MicroMPX_Decoder
chmod +x /opt/micrompx/MicroMPX_Decoder

# Install service
rm -f /etc/systemd/system/micrompx.service
wget https://raw.githubusercontent.com/oszuidwest/rpi-umpx-decoder/main/micrompx.service -O /etc/systemd/system/micrompx.service
systemctl daemon-reload
systemctl enable micrompx
service micrompx restart
