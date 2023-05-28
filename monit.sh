#!/bin/bash

# Define the process and URLs
PROCESS="/opt/micrompx/MicroMPX_Decoder"
GENERAL_HEARTBEAT="https://heartbeat.uptimerobot.com/xxxxx"
MICROMPX_MONITOR="https://heartbeat.uptimerobot.com/xxxxxxx"  # Update this as per your requirements

# Define the cron jobs
CRON_JOBS=(
"* * * * * /usr/bin/curl -s $GENERAL_HEARTBEAT > /dev/null"
"* * * * * pgrep -f $PROCESS > /dev/null && /usr/bin/curl -s $MICROMPX_MONITOR > /dev/null"
)

# Check each cron job and add it if it doesn't exist
for job in "${CRON_JOBS[@]}"; do
    # Check for existing crontab
    if crontab -l 2>/dev/null; then
        # Check if cron job exists
        if ! crontab -l | grep -q "$job"; then
            (crontab -l; echo "$job") | crontab -
        fi
    else
        # If no existing crontab, add job
        echo "$job" | crontab -
    fi
done
