#!/usr/bin/env bash

# Configuration
INSTALL_DIR="/opt/micrompx"
LOG_DIR="/home/micrompx/.MicroMPX_Decoder.log"
RAMDISK_PATH="/mnt/ramdisk"
REPO_BASE="https://raw.githubusercontent.com/oszuidwest/rpi-umpx-decoder/main"

# Set-up the functions library
FUNCTIONS_LIB_PATH="/tmp/functions.sh"
FUNCTIONS_LIB_URL="https://raw.githubusercontent.com/oszuidwest/bash-functions/main/common-functions.sh"

# Set-up MicroMPX
MICROMPX_DECODER_PATH="${INSTALL_DIR}/MicroMPX_Decoder"
MICROMPX_DECODER_URL="https://download.thimeo.com/MicroMPX_Decoder_ARM64_1021"
MICROMPX_SERVICE_PATH="/etc/systemd/system/micrompx.service"
MICROMPX_SERVICE_URL="${REPO_BASE}/micrompx.service"

# Set-up RAM disk
RAMDISK_SERVICE_PATH="/etc/systemd/system/ramdisk.service"
RAMDISK_SERVICE_URL="${REPO_BASE}/ramdisk.service"

# General Raspberry Pi configuration
CONFIG_FILE_PATHS=("/boot/firmware/config.txt" "/boot/config.txt")
FIRST_IP=$(hostname -I | awk '{print $1}')

# Start with a clean terminal
clear

# Remove old functions library and download the latest version
rm -f "$FUNCTIONS_LIB_PATH"
if ! curl -s -o "$FUNCTIONS_LIB_PATH" "$FUNCTIONS_LIB_URL"; then
  echo -e "*** Failed to download functions library. Please check your network connection! ***"
  exit 1
fi

# Source the functions file
# shellcheck source=/tmp/functions.sh
source "$FUNCTIONS_LIB_PATH"

# Set color variables
set_colors

# Check if running as root
check_user_privileges privileged

# Check if this is Linux
is_this_linux
is_this_os_64bit

# Check if we are running on a Raspberry Pi 4 or newer
check_rpi_model 4
RPI_MODEL=$(get_rpi_model)

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
ask_user "LOG_RETENTION_DAYS" "7" "How many days should logs be kept (default: 7)" "int"

# Check and stop MicroMPX service if running
echo -e "${BLUE}►► Checking and stopping MicroMPX service if running...${NC}"
if systemctl is-active --quiet micrompx > /dev/null; then
  systemctl stop micrompx > /dev/null || { echo -e "${RED}Failed to stop the MicroMPX service. Please check the logs for more details.${NC}"; exit 1; }
else
  echo -e "${YELLOW}MicroMPX service is not running.${NC}"
fi

# Timezone configuration
set_timezone Europe/Amsterdam

# Update the OS
update_os silent

# Add user for micrompx
echo -e "${BLUE}►► Setting up micrompx user...${NC}"
if ! id -u micrompx > /dev/null; then
  useradd -m micrompx --home /home/micrompx --shell /usr/sbin/nologin --comment "micrompx daemon user" > /dev/null
  echo -e "${GREEN}✓ Created user 'micrompx'${NC}"
else
  echo -e "${YELLOW}User 'micrompx' already exists.${NC}"
fi

if groups micrompx | grep -q '\baudio\b' > /dev/null; then
  echo -e "${YELLOW}User 'micrompx' already in audio group.${NC}"
else
  usermod -aG audio micrompx > /dev/null
  echo -e "${GREEN}✓ Added 'micrompx' to audio group${NC}"
fi

# Install dependencies
install_packages silent libasound2 libsndfile1 wget

# Download MicroMPX from Thimeo
echo -e "${BLUE}►► Downloading and installing MicroMPX...${NC}"
mkdir -p "$INSTALL_DIR" > /dev/null
curl -s -o "$MICROMPX_DECODER_PATH" "$MICROMPX_DECODER_URL"
chmod +x "$MICROMPX_DECODER_PATH" > /dev/null
setcap CAP_NET_BIND_SERVICE=+eip "$MICROMPX_DECODER_PATH" > /dev/null
echo -e "${GREEN}✓ MicroMPX decoder installed${NC}"

# Add service
echo -e "${BLUE}►► Installing MicroMPX service...${NC}"
rm -f "$MICROMPX_SERVICE_PATH" > /dev/null
curl -s -o "$MICROMPX_SERVICE_PATH" "$MICROMPX_SERVICE_URL"
systemctl daemon-reload > /dev/null
systemctl enable micrompx > /dev/null
echo -e "${GREEN}✓ MicroMPX service installed${NC}"

# Add RAM disk
echo -e "${BLUE}►► Setting up RAM disk for logs...${NC}"
rm -f "$RAMDISK_SERVICE_PATH" > /dev/null
curl -s -o "$RAMDISK_SERVICE_PATH" "$RAMDISK_SERVICE_URL"
systemctl daemon-reload > /dev/null
systemctl enable ramdisk > /dev/null
systemctl start ramdisk
echo -e "${GREEN}✓ RAM disk service installed${NC}"

# Put MicroMPX logs on RAM disk
if [ -d "$LOG_DIR" ]; then
  echo -e "${YELLOW}Removing existing log directory...${NC}"
  rm -rf "$LOG_DIR"
fi
ln -s "$RAMDISK_PATH" "$LOG_DIR"
chown -R micrompx:micrompx "$RAMDISK_PATH"
echo -e "${GREEN}✓ Logs linked to RAM disk${NC}"

# Clean logs to save space on the RAM disk (MicroMPX does this every 30 days)
LOGS_CRONJOB="0 0 * * * find -L $LOG_DIR -type f -mtime +${LOG_RETENTION_DAYS} -exec rm {} \;"
echo -e "${BLUE}►► Setting up log file deletion cronjob...${NC}"
# Check if the crontab exists for the current user, create one if not
if ! crontab -l 2>/dev/null; then
  echo "" | crontab -
fi
# Remove any existing log cleanup jobs first
crontab -l 2>/dev/null | grep -v "find -L $LOG_DIR" | crontab -
# Add the new cron job
(crontab -l 2>/dev/null; echo "$LOGS_CRONJOB") | crontab -
echo -e "${GREEN}✓ Log cleanup scheduled (${LOG_RETENTION_DAYS} day retention)${NC}"

# Heartbeat monitoring
if [ "$ENABLE_HEARTBEAT" == "y" ]; then
  echo -e "${BLUE}►► Setting up heartbeat monitoring...${NC}"
  HEARTBEAT_CRONJOB="* * * * * wget --spider $HEARTBEAT_URL > /dev/null 2>&1"
  if ! crontab -l | grep -F -- "$HEARTBEAT_CRONJOB" > /dev/null; then
    (crontab -l 2>/dev/null; echo "$HEARTBEAT_CRONJOB") | crontab -
    echo -e "${GREEN}✓ Heartbeat monitoring configured${NC}"
  else
    echo -e "${YELLOW}Heartbeat monitoring already configured.${NC}"
  fi
fi

# Disable HDMI audio to use the mini-jack for monitoring
echo -e "${BLUE}►► Configuring audio settings...${NC}"
sed -i '/dtoverlay=vc4-fkms-v3d/ { /audio=off/! s/$/,audio=off/ }' "$CONFIG_FILE" > /dev/null
sed -i '/dtoverlay=vc4-kms-v3d/ { /noaudio/! s/$/,noaudio/ }' "$CONFIG_FILE" > /dev/null
echo -e "${GREEN}✓ HDMI audio disabled${NC}"

# Check if HiFiBerry is configured
echo -e "${BLUE}►► Checking for HiFiBerry audio device...${NC}"
if ! grep -q "^dtoverlay=hifiberry" "$CONFIG_FILE"; then
  echo -e "${RED}No HiFiBerry card configured in $CONFIG_FILE${NC}"
  echo -e "${RED}Please configure your HiFiBerry device before running this script.${NC}"
  echo -e "${YELLOW}Add the appropriate dtoverlay line to $CONFIG_FILE${NC}"
  echo -e "${YELLOW}Example: dtoverlay=hifiberry-dacplus${NC}\n"
  exit 1
fi
HIFIBERRY_OVERLAY=$(grep "^dtoverlay=hifiberry" "$CONFIG_FILE" | head -n1)
echo -e "${GREEN}✓ Found HiFiBerry configuration: ${HIFIBERRY_OVERLAY}${NC}"


# Post-installation information
echo -e "\n${GREEN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
echo -e "${GREEN}✓ Installation Complete!${NC}"
echo -e "${GREEN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}\n"

echo -e "${YELLOW}▸ Access MicroMPX:${NC}"
echo -e "  Web Interface: ${CYAN}http://${FIRST_IP}:8080${NC}"
echo -e "  Service Status: ${CYAN}systemctl status micrompx${NC}"

echo -e "\n${YELLOW}▸ Service Management:${NC}"
echo -e "  Start:   ${CYAN}systemctl start micrompx${NC}"
echo -e "  Stop:    ${CYAN}systemctl stop micrompx${NC}"
echo -e "  Restart: ${CYAN}systemctl restart micrompx${NC}"
echo -e "  Logs:    ${CYAN}journalctl -u micrompx -f${NC}"

echo -e "\n${YELLOW}▸ Important Notes:${NC}"
echo -e "  • Logs are stored in RAM disk to protect SD card"
echo -e "  • Logs are automatically cleaned after ${LOG_RETENTION_DAYS} days"
if [[ "$RPI_MODEL" != "5" ]]; then
  echo -e "  • Analog audio output is enabled for monitoring"
fi


if [[ "$ENABLE_HEARTBEAT" == "y" ]]; then
  echo -e "\n${YELLOW}▸ Heartbeat Monitoring:${NC}"
  echo -e "  URL: ${CYAN}${HEARTBEAT_URL}${NC}"
  echo -e "  Frequency: Every minute"
fi

# Reboot
echo -e "\n${GREEN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
echo -e "${YELLOW}System will reboot in 10 seconds...${NC}"
echo -e "${GREEN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
sleep 10
reboot