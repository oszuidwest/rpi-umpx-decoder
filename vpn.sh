#!/usr/bin/env bash

# Start with a clean terminal
clear

# Download the functions library
curl -s -o /tmp/functions.sh https://raw.githubusercontent.com/oszuidwest/bash-functions/main/common-functions.sh

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

# Print usage information if the script is run with the wrong number of arguments
if (( $# < 4 )); then
  echo "Usage: $0 SERVER_PUBLIC_IP SERVER_PUBLIC_KEY NETWORK RASPBERRY_ADDRESS"
  echo "All four arguments are mandatory."
  echo "Alternatively, these can be set as environment variables:"
  echo "  - SERVER_PUBLIC_IP"
  echo "  - SERVER_PUBLIC_KEY"
  echo "  - NETWORK"
  echo "  - RASPBERRY_ADDRESS"
  exit 1
fi

# Variables (Pass these as arguments or set as environment variables)
SERVER_PUBLIC_IP="${1:-${SERVER_PUBLIC_IP}}"
SERVER_PUBLIC_KEY="${2:-${SERVER_PUBLIC_KEY}}"
NETWORK="${3:-${NETWORK}}"
RASPBERRY_ADDRESS="${4:-${RASPBERRY_ADDRESS}}" 

# Paths
WIREGUARD_PATH="/etc/wireguard"
PRIVATE_KEY_PATH="${WIREGUARD_PATH}/privatekey"
PUBLIC_KEY_PATH="${WIREGUARD_PATH}/publickey"
CONFIGURATION_PATH="${WIREGUARD_PATH}/wg0.conf"

# Ensure required variables are set
for var in SERVER_PUBLIC_IP SERVER_PUBLIC_KEY NETWORK RASPBERRY_ADDRESS; do
  if [[ -z ${!var} ]]; then
    echo "Error: $var is not set."
    exit 1
  fi
done

# Ensure WireGuard is installed
update_os silent
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

# Ensure the server keys are readable and not empty
for key_path in PRIVATE_KEY_PATH PUBLIC_KEY_PATH; do
  if [[ ! -r ${!key_path} || ! -s ${!key_path} ]]; then
    echo "Error: The file at ${!key_path} is not readable or is empty."
    exit 1
  fi
done

# Read the generated private key
GENERATED_PRIVATE_KEY=$(<"$PRIVATE_KEY_PATH")

# Backup old configuration file if it exists
if [[ -f $CONFIGURATION_PATH ]]; then
  mv "$CONFIGURATION_PATH" "${CONFIGURATION_PATH}_old_$(date +%Y%m%d%H%M%S)"
fi

# Create the WireGuard configuration file
cat >"$CONFIGURATION_PATH" <<EOL
[Interface]
Address = $RASPBERRY_ADDRESS
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

# Bring up the WireGuard interface
if ! wg-quick up wg0; then
  echo "Error: Failed to bring up the WireGuard interface."
  exit 1
fi

# Ensure the WireGuard service is active
if ! systemctl is-active --quiet wg-quick@wg0; then
  echo "Error: The WireGuard service is not active."
  exit 1
fi

# Enable the WireGuard service on boot
systemctl enable wg-quick@wg0

echo "WireGuard VPN configuration completed!"
