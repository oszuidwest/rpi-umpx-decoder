#!/usr/bin/env bash

# Start with a clean terminal
clear

# Download the functions library
if ! curl -s -o /tmp/functions.sh https://raw.githubusercontent.com/oszuidwest/bash-functions/main/common-functions.sh; then
  echo -e  "*** Failed to download functions library. Please check your network connection! ***"
  exit 1
fi

# Source the functions file
source /tmp/functions.sh

# Set color variables
set_colors

# Check if running as root
are_we_root

# Check if this is Linux
is_this_linux
is_this_os_64bit

# Check if we are running on a Raspberry Pi 4 or newer
check_rpi_model 4

# Something fancy for the sysadmin
cat << "EOF"
 ______     _     ___          __       _     ______ __  __ 
|___  /    (_)   | \ \        / /      | |   |  ____|  \/  |
   / /_   _ _  __| |\ \  /\  / /__  ___| |_  | |__  | \  / |
  / /| | | | |/ _` | \ \/  \/ / _ \/ __| __| |  __| | |\/| |
 / /_| |_| | | (_| |  \  /\  /  __/\__ \ |_  | |    | |  | |
/_____\__,_|_|\__,_|   \/  \/ \___||___/\__| |_|    |_|  |_|
EOF

# Hi!
echo -e "${GREEN}⎎ MicroMPX Setup for Raspberry Pi${NC}\n\n"

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
set_timezone Europe/Amsterdam

# Update the OS
update_os silent

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
install_packages silent libasound2

# Download micrompx from Thimeo
echo -e "${BLUE}►► Downloading and installing MicroMPX...${NC}"
mkdir -p /opt/micrompx > /dev/null
curl -s -o /opt/micrompx/MicroMPX_Decoder https://download.thimeo.com/MicroMPX_Decoder_ARM64
chmod +x /opt/micrompx/MicroMPX_Decoder > /dev/null
setcap CAP_NET_BIND_SERVICE=+eip /opt/micrompx/MicroMPX_Decoder > /dev/null

# Add service
echo -e "${BLUE}►► Installing MicroMPX service...${NC}"
rm -f /etc/systemd/system/micrompx.service > /dev/null
curl -s -o /etc/systemd/system/micrompx.service https://raw.githubusercontent.com/oszuidwest/rpi-umpx-decoder/main/micrompx.service
systemctl daemon-reload > /dev/null
systemctl enable micrompx > /dev/null

# Heartbeat monitoring
ask_user "ENABLE_HEARTBEAT" "n" "Do you want to integrate heartbeat monitoring via UptimeRobot (y/n)" "y/n"
if [ "$ENABLE_HEARTBEAT" == "y" ]; then
  ask_user "HEARTBEAT_URL" "https://heartbeat.uptimerobot.com/xxx" "Enter the URL to get every minute for heartbeat monitoring" "str"

  # Add a cronjob that calls the HEARTBEAT_URL every minute
  echo -e "${BLUE}►► Setting up heartbeat monitoring cronjob...${NC}"
  (crontab -l 2>/dev/null; echo "* * * * * wget --spider $HEARTBEAT_URL > /dev/null 2>&1") | crontab -
  echo -e "${GREEN}Heartbeat monitoring cronjob added.${NC}"
fi

# Disable only the hdmi audio so we can use the minijack for monitoring
echo -e "${BLUE}►► Disabling onboard audio...${NC}"
readonly CONFIG_FILE="/boot/firmware/config.txt"
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
