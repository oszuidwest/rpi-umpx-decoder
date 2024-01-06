#!/bin/bash

# Start with a clean terminal
clear

# Download the functions library
if ! curl -f -Ss -o /tmp/functions.sh https://raw.githubusercontent.com/oszuidwest/bash-functions/main/common-functions.sh; then
  echo -e  "*** Failed to download functions library. Please check your network connection! ***"
  exit 1
fi

# Source the functions file
source /tmp/functions.sh

# Set color variables
set_colors

# Start with a clean terminal
clear

# Check if running as root
are_we_root

# Check if this is Linux
is_this_linux
is_this_os_64bit

# Check if we are running on a Raspberry Pi 3 or newer
check_rpi_model 3

# Ask for input for variables
ask_user "SERVER_PUBLIC_IP" "8.8.8.8" "Enter the public ip-address of the Wireguard server" "str"
ask_user "SERVER_PUBLIC_KEY" "GQ4G7V+uRFRbqzYTgNHLd58o+RNPUW99L7Nc7mTt2Hs=" "Enter the public key of the Wirguard server" "str"
ask_user "NETWORK" "172.18.1.0/24" "Enter the network range you want to allow to connect" "str"
ask_user "RASPBERRY_ADDRESS" "172.18.1.2" "Enter the private ip-address this device should have" "str"

# Paths
WIREGUARD_PATH="/etc/wireguard"
PRIVATE_KEY_PATH="${WIREGUARD_PATH}/privatekey"
PUBLIC_KEY_PATH="${WIREGUARD_PATH}/publickey"
CONFIGURATION_PATH="${WIREGUARD_PATH}/wg0.conf"

# Ensure WireGuard is installed
install_packages silent wireguard

# Generate server keys if they do not exist
if [[ ! -f $PRIVATE_KEY_PATH || ! -f $PUBLIC_KEY_PATH ]]; then
  echo "Server keys are missing. Generating new keys..."
  umask 077
  if ! wg genkey | tee "$PRIVATE_KEY_PATH" | wg pubkey > "$PUBLIC_KEY_PATH"; then
    echo "Error: Failed to generate keys."
    exit 1
  fi
fi

# Read the generated keys
GENERATED_PRIVATE_KEY=$(<"$PRIVATE_KEY_PATH")
GENERATED_PUBLIC_KEY=$(<"$PUBLIC_KEY_PATH")

# Backup old configuration file if it exists
if [[ -f $CONFIGURATION_PATH ]]; then
  mv "$CONFIGURATION_PATH" "${CONFIGURATION_PATH}_old_$(date +%Y%m%d%H%M%S)"
fi

# Create the WireGuard configuration file
cat >"$CONFIGURATION_PATH" <<EOL
[Interface]
Address = $RASPBERRY_ADDRESS/32
PrivateKey = $GENERATED_PRIVATE_KEY

[Peer]
PublicKey = $SERVER_PUBLIC_KEY
Endpoint = $SERVER_PUBLIC_IP:51820
AllowedIPs = $NETWORK
PersistentKeepalive = 25
EOL

# Ensure the WireGuard configuration file is readable and not empty
if [[ ! -r $CONFIGURATION_PATH || ! -s $CONFIGURATION_PATH ]]; then
  echo "Error: The WireGuard configuration file is not readable or is empty."
  exit 1
fi

# Check if the WireGuard interface needs to be enabled on boot
if ! systemctl is-enabled --quiet wg-quick@wg0
then
    echo -e "${BLUE}►► Enabling the wg0 interface on boot...${NC}"
    systemctl enable wg-quick@wg0
fi

# Bring up the WireGuard interface
if ip link show wg0 &> /dev/null; then
    echo -e "${BLUE}►► Restarting wg0...${NC}"
    wg-quick down wg0
    wg-quick up wg0
else
    echo -e "${BLUE}►► Bringing wg0 up...${NC}"
    wg-quick up wg0
fi

# Fin 
echo -e "\n${GREEN}✓ Success!${NC}"
echo -e "There should now be an interface named ${BOLD}wg0${NC} on this machine."
echo -e "The IP of the WireGuard interface is ${BOLD}$RASPBERRY_ADDRESS${NC}"
echo -e "The public key to put in the server is ${BOLD}$GENERATED_PUBLIC_KEY${NC}\n"
