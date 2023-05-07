#!/bin/bash

echo -e "\e[1;32mMicroMPX Setup for Raspberry Pi 4\e[0m"

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

# Check and stop micrompx service if running
echo "Checking and stopping MicroMPX service if running..."
if systemctl is-active --quiet micrompx; then
  systemctl stop micrompx || { echo "Failed to stop the MicroMPX service. Please check the logs for more details."; exit 1; }
else
  echo "MicroMPX service is not running. Assuming fresh install."
fi

echo "Expanding filesystem..."
raspi-config --expand-rootfs

echo "Setting timezone to Europe/Amsterdam..."
ln -fs /usr/share/zoneinfo/Europe/Amsterdam /etc/localtime
dpkg-reconfigure -f noninteractive tzdata

echo "Updating all packages..."
apt -qq -y update
apt -qq -y upgrade
apt -qq -y autoremove

echo "Adding micrompx user if it doesn't exist..."
if ! id -u micrompx > /dev/null 2>&1; then
  useradd -m micrompx --home /home/micrompx --shell /usr/sbin/nologin --comment "micrompx daemon user"
fi

echo "Installing dependencies..."
apt -qq -y install libasound2

echo "Downloading and installing MicroMPX..."
mkdir -p /opt/micrompx
wget https://www.stereotool.com/download/MicroMPX_Decoder_ARM64 -O /opt/micrompx/MicroMPX_Decoder
chmod +x /opt/micrompx/MicroMPX_Decoder
setcap CAP_NET_BIND_SERVICE=+eip /opt/micrompx/MicroMPX_Decoder

echo "Installing MicroMPX service..."
rm -f /etc/systemd/system/micrompx.service
wget https://raw.githubusercontent.com/oszuidwest/rpi-umpx-decoder/main/micrompx.service -O /etc/systemd/system/micrompx.service
systemctl daemon-reload
systemctl enable micrompx
service micrompx restart

echo "Disabling onboard audio..."
CONFIG_FILE="/boot/config.txt"
sed -i '/^[^#]*dtparam=audio=on/s/^/#/' "$CONFIG_FILE"
sed -i '/dtoverlay=vc4-fkms-v3d/ { /audio=off/! s/$/,audio=off/ }' "$CONFIG_FILE"
sed -i '/dtoverlay=vc4-kms-v3d/ { /noaudio/! s/$/,noaudio/ }' "$CONFIG_FILE"

echo "Configuring device tree overlay for HifiBerry..."
echo "Enter the number corresponding to your device:"
echo "1. DAC FOR RASPBERRY PI 1/DAC+ LIGHT/DAC ZERO/MINIAMP/BEOCREATE/DAC+ DSP/DAC+ RTC"
echo "2. DAC+ STANDARD/PRO/AMP2"
echo "3. DAC2 HD"
echo "4. DAC+ ADC"
echo "5. DAC+ ADC PRO"
echo "6. DIGI+"
echo "7. DIGI+ PRO"

read -r device_number
case $device_number in
    1) overlay="hifiberry-dac" ;;
    2) overlay="hifiberry-dacplus" ;;
    3) overlay="hifiberry-dacplushd" ;;
    4) overlay="hifiberry-dacplusadc" ;;
    5) overlay="hifiberry-dacplusadcpro" ;;
    6) overlay="hifiberry-digi" ;;
    7) overlay="hifiberry-digi-pro" ;;
    *) echo "Invalid input, exiting."; exit 1 ;;
esac

if ! grep -q "dtoverlay=$overlay" $CONFIG_FILE; then
    echo "dtoverlay=$overlay" >> $CONFIG_FILE
fi

echo "Checking Linux version and disabling onboard EEPROM if necessary..."
kernel_version=$(uname -r | awk -F. '{print $1 "." $2}')

if [ "$(printf "%s\\n" "5.4" "$kernel_version" | sort -V | head -n1)" = "5.4" ] && [ "$kernel_version" != "5.4" ]; then
    if ! grep -q 'force_eeprom_read=0' $CONFIG_FILE; then
        echo "force_eeprom_read=0" >> $CONFIG_FILE
    fi
fi

echo "Configuration updated. Please reboot your system for the changes to take effect."