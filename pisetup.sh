#!/bin/bash

# Function that checks if this a supported platform
function check_platform() {
  if ! grep "Raspberry Pi 4" /proc/device-tree/model &> /dev/null; then
    echo -e "\e[1;31;5m** NOT RUNNING ON A RASPBERRY PI 4 **\e[0m"
    read -rp $'\e[3m\e[33mThis script is only tested on a Raspberry Pi 4. Press Enter to continue anyway...\e[0m\n'
  fi
}

# Check if running as root
if [ "$(id -u)" -ne 0 ]; then
  echo "This script must be run as root. Please use 'sudo bash pisetup.sh' to run it."
  exit 1
fi

# Expand the filesystem
raspi-config --expand-rootfs

# Set the timezone
ln -fs /usr/share/zoneinfo/Europe/Amsterdam /etc/localtime
dpkg-reconfigure -f noninteractive tzdata

# Update all packages
apt update -y -qq
apt upgrade -y -qq
