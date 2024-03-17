#!/usr/bin/env bash

# Define paths and URLs
FUNCTIONS_SH_PATH="/tmp/functions.sh"
FUNCTIONS_SH_URL="https://raw.githubusercontent.com/oszuidwest/bash-functions/main/common-functions.sh"

MICROMPX_DECODER_PATH="/opt/micrompx/MicroMPX_Decoder"
MICROMPX_DECODER_URL="https://download.thimeo.com/MicroMPX_Decoder_ARM64"
MICROMPX_SERVICE_PATH="/etc/systemd/system/micrompx.service"
MICROMPX_SERVICE_URL="https://raw.githubusercontent.com/oszuidwest/rpi-umpx-decoder/main/micrompx.service"
LOG_DIR="/home/micrompx/.MicroMPX_Decoder.log" # Log directory

RAMDISK_SERVICE_PATH="/etc/systemd/system/ramdisk.service"
RAMDISK_SERVICE_URL="https://raw.githubusercontent.com/oszuidwest/rpi-umpx-decoder/main/ramdisk.service"
RAMDISK_PATH="/mnt/ramdisk" # RAM disk path

CONFIG_FILE_PATHS=("/boot/firmware/config.txt" "/boot/config.txt") # Config file paths
FIRST_IP=$(hostname -I | awk '{print $1}') # Determine the first IP address upfront

# Start with a clean terminal
clear

# Remove old functions library and download the latest version
rm -f "$FUNCTIONS_SH_PATH"
if ! curl -s -o "$FUNCTIONS_SH_PATH" "$FUNCTIONS_SH_URL"; then
  echo -e "*** Failed to download functions library. Please check your network connection! ***"
  exit 1
fi

# Source the functions file
source "$FUNCTIONS_SH_PATH"

# Set color variables
set_colors

# Check if running as root
are_we_root

# Check if this is Linux
is_this_linux
is_this_os_64bit

# Check if we are running on a Raspberry Pi 4 or newer
check_rpi_model 4

# Determine the correct config file path
CONFIG_FILE=""
for path in "${CONFIG_FILE_PATHS[@]}"; do
  if [ -f "$path" ]; then
    CONFIG_FILE="$path"
    break
  fi
done

if [ -z "$CONFIG_FILE" ]; then
  echo -e "${RED}Error: config.txt not found in known locations.${NC}"
  exit 1
fi

# Banner
cat << "EOF"
 ______     _     ___          __       _     ______ __  __ 
|___  /    (_)   | \ \        / /      | |   |  ____|  \/  |
   / /_   _ _  __| |\ \  /\  / /__  ___| |_  | |__  | \  / |
  / /| | | | |/ _` | \ \/  \/ / _ \/ __| __| |  __| | |\/| |
 / /_| |_| | | (_| |  \  /\  /  __/\__ \ |_  | |    | |  | |
/_____\__,_|_|\__,_|   \/  \/ \___||___/\__| |_|    |_|  |_|
EOF

# Greeting
echo -e "${GREEN}⎎ MicroMPX Setup for Raspberry Pi${NC}\n\n"
ask_user "ENABLE_HEARTBEAT" "n" "Do you want to integrate heartbeat monitoring via UptimeRobot (y/n)" "y/n"
if [ "$ENABLE_HEARTBEAT" == "y" ]; then
  ask_user "HEARTBEAT_URL" "https://heartbeat.uptimerobot.com/xxx" "Enter the URL to get every minute for heartbeat monitoring" "str"
fi

# Check and stop MicroMPX service if running
echo -e "${BLUE}►► Checking and stopping MicroMPX service if running...${NC}"
if systemctl is-active --quiet micrompx > /dev/null; then
  systemctl stop micrompx > /dev/null || { echo -e "${RED}Failed to stop the MicroMPX service. Please check the logs for more details.${NC}"; exit 1; }
else
  echo -e "${YELLOW}MicroMPX service is not running. Assuming this is a fresh install.${NC}"
fi

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

# Install dependencies for MicroMPX
install_packages silent libasound2 libsndfile1

# Download MicroMPX from Thimeo
echo -e "${BLUE}►► Downloading and installing MicroMPX...${NC}"
mkdir -p /opt/micrompx > /dev/null
curl -s -o "$MICROMPX_DECODER_PATH" "$MICROMPX_DECODER_URL"
chmod +x "$MICROMPX_DECODER_PATH" > /dev/null
setcap CAP_NET_BIND_SERVICE=+eip "$MICROMPX_DECODER_PATH" > /dev/null

# Add service
echo -e "${BLUE}►► Installing MicroMPX service...${NC}"
rm -f "$MICROMPX_SERVICE_PATH" > /dev/null
curl -s -o "$MICROMPX_SERVICE_PATH" "$MICROMPX_SERVICE_URL"
systemctl daemon-reload > /dev/null
systemctl enable micrompx > /dev/null

# Add RAM disk
echo -e "${BLUE}►► Setting up RAM disk for logs...${NC}"
rm -f "$RAMDISK_SERVICE_PATH" > /dev/null
curl -s -o "$RAMDISK_SERVICE_PATH" "$RAMDISK_SERVICE_URL"
systemctl daemon-reload > /dev/null
systemctl enable ramdisk > /dev/null
systemctl start ramdisk

# Put MicroMPX logs on RAM disk
echo -e "${BLUE}►► Putting MicroMPX logs on the RAM disk...${NC}"
if [ -d "$LOG_DIR" ]; then
  echo -e "${YELLOW}Log directory exists. Removing it before creating the symlink.${NC}"
  rm -rf "$LOG_DIR"
fi
ln -s "$RAMDISK_PATH" "$LOG_DIR"
chown -R micrompx:micrompx "$RAMDISK_PATH"

# Clean logs every 7 days to save space on the RAM disk (MicroMPX does this every 30 days)
LOGS_CRONJOB="0 0 * * * find -L $LOG_DIR -type f -mtime +7 -exec rm {} \;"
echo -e "${BLUE}►► Setting up log file deletion cronjob...${NC}"
if ! crontab -l | grep -F -- "$LOGS_CRONJOB" > /dev/null; then
  (crontab -l 2>/dev/null; echo "$LOGS_CRONJOB") | crontab -
else
  echo -e "${YELLOW}Log file deletion cronjob already exists. No changes made.${NC}"
fi

# Heartbeat monitoring
if [ "$ENABLE_HEARTBEAT" == "y" ]; then
  echo -e "${BLUE}►► Setting up heartbeat monitoring...${NC}"
  HEARTBEAT_CRONJOB="* * * * * wget --spider $HEARTBEAT_URL > /dev/null 2>&1"
  if ! crontab -l | grep -F -- "$HEARTBEAT_CRONJOB" > /dev/null; then
    (crontab -l 2>/dev/null; echo "$HEARTBEAT_CRONJOB") | crontab -
  else
    echo -e "${YELLOW}Heartbeat monitoring cronjob already exists. No changes made.${NC}"
  fi
fi

# Disable HDMI audio to use the mini-jack for monitoring
echo -e "${BLUE}►► Disabling onboard audio...${NC}"
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
if ! grep -q "dtoverlay=$overlay" "$CONFIG_FILE" > /dev/null; then
  echo "dtoverlay=$overlay" >> "$CONFIG_FILE"
fi

# Apply HifiBerry kernel fix if needed
echo -e "${BLUE}►► Checking Linux version and disabling onboard EEPROM if necessary...${NC}"
kernel_version=$(uname -r | awk -F. '{print $1 "." $2}')
if [ "$(printf '%s\n' "5.4" "$kernel_version" | sort -V | head -n1)" = "5.4" ] && [ "$kernel_version" != "5.4" ]; then
  grep -q 'force_eeprom_read=0' "$CONFIG_FILE" || echo "force_eeprom_read=0" >> "$CONFIG_FILE"
fi

# Reboot
echo -e "\n\n${GREEN}✓ Setup is complete! Your Raspberry Pi will reboot in 10 seconds.${NC}"
echo -e "Access the MicroMPX interface at http://${FIRST_IP}:8080 after the reboot."
sleep 10
reboot
