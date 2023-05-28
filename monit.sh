#!/bin/bash

# Define the process and URLs
PROCESS="/opt/micrompx/MicroMPX_Decoder"
GENERAL_HEARTBEAT="https://heartbeat.uptimerobot.com/xxx"
MICROMPX_MONITOR="https://heartbeat.uptimerobot.com/xxx"  # Update this as per your requirements

# Define the cron jobs
CRON_JOBS=(
"*/1 * * * * wget --spider $GENERAL_HEARTBEAT >/dev/null 2>&1"
"*/1 * * * * pgrep -f $PROCESS > /dev/null && wget --spider $MICROMPX_MONITOR >/dev/null 2>&1"
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
