#!/usr/bin/env bash

# Set some colors
readonly GREEN='\033[1;32m'
readonly RED='\033[1;31m'
readonly YELLOW='\033[0;33m'
readonly BLUE='\033[1;34m'
readonly NC='\033[0m' # No Color

# Hi!
printf "${GREEN}⎎ MicroMPX Setup for Raspberry Pi 4${NC}\n\n"

# Function that checks if this is a supported platform
check_platform() {
  if ! grep -q "Raspberry Pi 4" /proc/device-tree/model > /dev/null; then
    printf "${RED}** NOT RUNNING ON A RASPBERRY PI 4 **${NC}\n"
    printf "${YELLOW}This script is only tested on a Raspberry Pi 4. Press Enter to continue anyway...${NC}\n"
	read -r
  fi
}

# Check if running as root
if [[ "$(id -u)" -ne 0 ]]; then
  printf "${RED}This script must be run as root. Please run 'sudo su' first.${NC}\n"
  exit 1
fi

# Check if we are running on a Raspberry PI 4
check_platform

# Check and stop micrompx service if running
printf "${BLUE}►► Checking and stopping MicroMPX service if running...${NC}\n\n"
if systemctl is-active --quiet micrompx > /dev/null; then
  systemctl stop micrompx > /dev/null || { printf "${RED}Failed to stop the MicroMPX service. Please check the logs for more details.${NC}\n"; exit 1; }
else
  printf "${YELLOW}MicroMPX service is not running. This it either a fresh install or the stop succeeded.${NC}\n"
fi

# Expand filesystem
printf "${BLUE}►► Expanding filesystem...${NC}\n\n"
raspi-config --expand-rootfs > /dev/null

# Timezone configuration
printf "${BLUE}►► Setting timezone to Europe/Amsterdam...${NC}\n\n"
ln -fs /usr/share/zoneinfo/Europe/Amsterdam /etc/localtime > /dev/null
dpkg-reconfigure -f noninteractive tzdata > /dev/null

# Update the OS
printf "${BLUE}►► Updating all packages...${NC}\n\n"
apt -qq -y update > /dev/null
apt -qq -y full-upgrade > /dev/null
apt -qq -y autoremove > /dev/null

# Add user for micrompx
printf "${BLUE}►► Adding micrompx user if it doesn't exist...${NC}\n\n"
if ! id -u micrompx > /dev/null; then
  useradd -m micrompx --home /home/micrompx --shell /usr/sbin/nologin --comment "micrompx daemon user" > /dev/null
fi

printf "${BLUE}Checking if the user 'micrompx' is a member of the 'audio' group${NC}\n"
if groups micrompx | grep -q '\baudio\b' > /dev/null; then
    printf "${YELLOW}User 'micrompx' is already a member of the 'audio' group. Not doing anything.${NC}\n"
else
    printf "${YELLOW}User 'micrompx' is not a member of the 'audio' group. Adding them to the group now...${NC}\n"
    usermod -aG audio micrompx > /dev/null
fi

# Install dependencies for micrompx
printf "${BLUE}►► Installing dependencies...${NC}\n\n"
apt -qq -y install libasound2 > /dev/null

# Download micrompx for Thimeo
printf "${BLUE}►► Downloading and installing MicroMPX...${NC}\n\n"
mkdir -p /opt/micrompx > /dev/null
wget -q https://www.stereotool.com/download/MicroMPX_Decoder_ARM64 -O /opt/micrompx/MicroMPX_Decoder > /dev/null
chmod +x /opt/micrompx/MicroMPX_Decoder > /dev/null
setcap CAP_NET_BIND_SERVICE=+eip /opt/micrompx/MicroMPX_Decoder > /dev/null

# Add service
printf "${BLUE}►► Installing MicroMPX service...${NC}\n\n"
rm -f /etc/systemd/system/micrompx.service > /dev/null
wget -q https://raw.githubusercontent.com/oszuidwest/rpi-umpx-decoder/main/micrompx.service -O /etc/systemd/system/micrompx.service > /dev/null
systemctl daemon-reload > /dev/null
systemctl enable micrompx > /dev/null

# Disable only the hdmi audio so we can use the minijack for monitoring
printf "${BLUE}►► Disabling onboard audio...${NC}\n\n"
readonly CONFIG_FILE="/boot/config.txt"
sed -i '/dtoverlay=vc4-fkms-v3d/ { /audio=off/! s/$/,audio=off/ }' "$CONFIG_FILE" > /dev/null
sed -i '/dtoverlay=vc4-kms-v3d/ { /noaudio/! s/$/,noaudio/ }' "$CONFIG_FILE" > /dev/null

# Configure the HifiBerry
printf "${BLUE}►► Configuring device tree overlay for HifiBerry...${NC}\n\n"
printf "Enter the number corresponding to your device:\n"
printf "1. DAC FOR RASPBERRY PI 1/DAC+ LIGHT/DAC ZERO/MINIAMP/BEOCREATE/DAC+ DSP/DAC+ RTC\n"
printf "2. DAC+ STANDARD/PRO/XLR\n"
printf "3. DAC2 HD\n"
printf "4. DAC+ ADC\n"
printf "5. DAC+ ADC PRO\n"
printf "6. DIGI+\n"
printf "7. DIGI+ PRO\n"

read -r device_number
case $device_number in
    1) overlay="hifiberry-dac" ;;
    2) overlay="hifiberry-dacplus" ;;
    3) overlay="hifiberry-dacplushd" ;;
    4) overlay="hifiberry-dacplusadc" ;;
    5) overlay="hifiberry-dacplusadcpro" ;;
    6) overlay="hifiberry-digi" ;;
    7) overlay="hifiberry-digi-pro" ;;
    *) printf "${RED}Invalid input, exiting.${NC}\n"; exit 1 ;;
esac

if ! grep -q "dtoverlay=$overlay" $CONFIG_FILE > /dev/null; then
    printf "dtoverlay=$overlay" >> $CONFIG_FILE
fi

# Apply HifiBerry kernel fix is needed
printf "${BLUE}►► Checking Linux version and disabling onboard EEPROM if necessary...${NC}\n\n"
kernel_version=$(uname -r | awk -F. '{print $1 "." $2}')

if [ "$(printf "%s\\n" "5.4" "$kernel_version" | sort -V | head -n1)" = "5.4" ] && [ "$kernel_version" != "5.4" ]; then
    if ! grep -q 'force_eeprom_read=0' $CONFIG_FILE > /dev/null; then
        printf "force_eeprom_read=0" >> $CONFIG_FILE
    fi
fi

# Fin!
printf "${GREEN}✓ Configuration updated. Please reboot your system for the changes to take effect.${NC}\n"