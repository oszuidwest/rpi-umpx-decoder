#!/bin/bash

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
