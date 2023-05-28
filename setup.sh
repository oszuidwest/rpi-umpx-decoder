#!/usr/bin/env bash

# Set some colors
readonly GREEN='\033[1;32m'
readonly RED='\033[1;31m'
readonly YELLOW='\033[0;33m'
readonly BLUE='\033[1;34m'
readonly NC='\033[0m' # No Color

# Something fancy for the sysadmin
clear
cat << "EOF"
 ______     _     ___          __       _     ______ __  __ 
|___  /    (_)   | \ \        / /      | |   |  ____|  \/  |
   / /_   _ _  __| |\ \  /\  / /__  ___| |_  | |__  | \  / |
  / /| | | | |/ _` | \ \/  \/ / _ \/ __| __| |  __| | |\/| |
 / /_| |_| | | (_| |  \  /\  /  __/\__ \ |_  | |    | |  | |
/_____\__,_|_|\__,_|   \/  \/ \___||___/\__| |_|    |_|  |_|
EOF

# Hi!
echo -e "${GREEN}⎎ MicroMPX Setup for Raspberry Pi 4${NC}\n\n"

# Function that checks if this is a supported platform
check_platform() {
  if ! grep -q "Raspberry Pi 4" /proc/device-tree/model > /dev/null; then
    echo -e "${RED}** NOT RUNNING ON A RASPBERRY PI 4 **${NC}"
    echo -e "${YELLOW}This script is only tested on a Raspberry Pi 4. Press Enter to continue anyway...${NC}"
    read -r
  fi
}

# Check if running as root
if [[ "$(id -u)" -ne 0 ]]; then
  echo -e "${RED}This script must be run as root. Please run 'sudo su' first.${NC}"
  exit 1
fi

# Check if we are running on a Raspberry PI 4
check_platform

# Check and stop micrompx service if running
echo -e "${BLUE}►► Checking and stopping MicroMPX service if running...${NC}"
if systemctl is-active --quiet micrompx > /dev/null; then
  systemctl stop micrompx > /dev/null || { echo -e "${RED}Failed to stop the MicroMPX service. Please check the logs for more details.${NC}"; exit 1; }
else
  echo -e "${YELLOW}MicroMPX service is not running. This it either a fresh install or the stop succeeded.${NC}"
fi

# Expand filesystem
echo -e "${BLUE}►► Expanding filesystem...${NC}"
raspi-config --expand-rootfs > /dev/null

# Timezone configuration
echo -e "${BLUE}►► Setting timezone to Europe/Amsterdam...${NC}"
ln -fs /usr/share/zoneinfo/Europe/Amsterdam /etc/localtime > /dev/null
dpkg-reconfigure -f noninteractive tzdata > /dev/null

# Update the OS
echo -e "${BLUE}►► Updating all packages...${NC}"
apt -qq -y update > /dev/null 2>&1
apt -qq -y full-upgrade > /dev/null 2>&1
apt -qq -y autoremove > /dev/null 2>&1

# Add user for micrompx
echo -e "${BLUE}►► Adding micrompx user if it doesn't exist...${NC}"
if ! id -u micrompx > /dev/null; then
  useradd -m micrompx --home /home/micrompx --shell /usr/sbin/nologin --comment "micrompx daemon user" > /dev/null
fi

echo -e "${BLUE}►► Checking if the user 'micrompx' is a member of the 'audio' group${NC}"
if groups micrompx | grep -q '\baudio\b' > /dev/null; then
    echo -e "${YELLOW}User 'micrompx' is already a member of the 'audio' group. Not doing anything.${NC}"
else
    echo -e "${YELLOW}User 'micrompx' is not a member of the 'audio' group. Adding them to the group now...${NC}"
    usermod -aG audio micrompx > /dev/null
fi

# Install dependencies for micrompx
echo -e "${BLUE}►► Installing dependencies...${NC}"
apt -qq -y install libasound2 > /dev/null 2>&1

# Download micrompx from Thimeo
echo -e "${BLUE}►► Downloading and installing MicroMPX...${NC}"
mkdir -p /opt/micrompx > /dev/null
wget -q https://www.stereotool.com/download/MicroMPX_Decoder_ARM64 -O /opt/micrompx/MicroMPX_Decoder > /dev/null
chmod +x /opt/micrompx/MicroMPX_Decoder > /dev/null
setcap CAP_NET_BIND_SERVICE=+eip /opt/micrompx/MicroMPX_Decoder > /dev/null

# Add service
echo -e "${BLUE}►► Installing MicroMPX service...${NC}"
rm -f /etc/systemd/system/micrompx.service > /dev/null
wget -q https://raw.githubusercontent.com/oszuidwest/rpi-umpx-decoder/main/micrompx.service -O /etc/systemd/system/micrompx.service > /dev/null
systemctl daemon-reload > /dev/null
systemctl enable micrompx > /dev/null

# Disable only the hdmi audio so we can use the minijack for monitoring
echo -e "${BLUE}►► Disabling onboard audio...${NC}"
readonly CONFIG_FILE="/boot/config.txt"
sed -i '/dtoverlay=vc4-fkms-v3d/ { /audio=off/! s/$/,audio=off/ }' "$CONFIG_FILE" > /dev/null
sed -i '/dtoverlay=vc4-kms-v3d/ { /noaudio/! s/$/,noaudio/ }' "$CONFIG_FILE" > /dev/null

# Configure the HifiBerry
echo -e "${BLUE}►► Configuring device tree overlay for HifiBerry...${NC}"
echo -e "Enter the number corresponding to your device:"
echo -e "1. DAC FOR RASPBERRY PI 1/DAC+ LIGHT/DAC ZERO/MINIAMP/BEOCREATE/DAC+ DSP/DAC+ RTC"
echo -e "2. DAC+ STANDARD/PRO/XLR"
echo -e "3. DAC2 HD"
echo -e "4. DAC+ ADC"
echo -e "5. DAC+ ADC PRO"
echo -e "6. DIGI+"
echo -e "7. DIGI+ PRO"

read -r device_number
case $device_number in
    1) overlay="hifiberry-dac" ;;
    2) overlay="hifiberry-dacplus" ;;
    3) overlay="hifiberry-dacplushd" ;;
    4) overlay="hifiberry-dacplusadc" ;;
    5) overlay="hifiberry-dacplusadcpro" ;;
    6) overlay="hifiberry-digi" ;;
    7) overlay="hifiberry-digi-pro" ;;
    *) echo -e "${RED}Invalid input, exiting.${NC}"; exit 1 ;;
esac

if ! grep -q "dtoverlay=$overlay" $CONFIG_FILE > /dev/null; then
    echo -e "dtoverlay=$overlay" >> $CONFIG_FILE
fi

# Apply HifiBerry kernel fix is needed
echo -e "${BLUE}►► Checking Linux version and disabling onboard EEPROM if necessary...${NC}"
kernel_version=$(uname -r | awk -F. '{print $1 "." $2}')
if [ "$(printf '%s\n' "5.4" "$kernel_version" | sort -V | head -n1)" = "5.4" ] && [ "$kernel_version" != "5.4" ]; then
    grep -q 'force_eeprom_read=0' $CONFIG_FILE || echo -e "force_eeprom_read=0" >> $CONFIG_FILE
fi

# Reboot
echo -e "\n\n${GREEN}✓ Setup is complete! Your Raspberry Pi will reboot in 10 seconds.${NC}"
sleep 10
reboot
