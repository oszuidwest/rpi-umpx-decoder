#!/bin/bash

# URL of the JSON file
url="https://uptimerobot.com/inc/files/ips/IPRanges.json"

# Destination config file
config_file="config.txt"

# Use curl to get the JSON, then use jq to parse it and format the prefixes as a single line
ip_ranges=$(curl -s "$url" | jq -r '.prefixes[] | (.ipv4Prefix // .ipv6Prefix)' | sed '$!s/$/,/' | tr '\n' ' ')

# Write the line to the config file
echo "$ip_ranges" > "$config_file"

echo "IP ranges written to $config_file"
