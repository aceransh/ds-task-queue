#!/bin/bash
BROKER_URL="http://localhost:8080"
WORKER_ID="${1:-worker-1}"

while true; do
    RESPONSE=$(curl -s -X POST "$BROKER_URL/poll" \
    -H "Content-Type: application/json" \
    -d "{\"worker_id\":\"$WORKER_ID\"}")

    if [[ -z "$RESPONSE" ]]; then
        sleep 1
        continue
    fi

    JOB_ID=$(echo "$RESPONSE" | jq -r '.id')
    LEASE_ID=$(echo "$RESPONSE" | jq -r '.lease_id')

    ACTION=$((RANDOM % 10))

    if (( ACTION <= 5)); then
        echo "[worker $WORKER_ID] ACK job $JOB_ID lease $LEASE_ID"
        curl -s -X POST "$BROKER_URL/ack" \
        -H "Content-Type: application/json" \
        -d "{\"worker_id\":\"$WORKER_ID\",\"job_id\":\"$JOB_ID\",\"lease_id\":$LEASE_ID}"
    
    elif ((ACTION <= 7)); then
        echo "[worker $WORKER_ID] FAIL job $JOB_ID lease $LEASE_ID"
        curl -s -X POST "$BROKER_URL/fail" \
        -H "Content-Type: application/json" \
        -d "{\"worker_id\":\"$WORKER_ID\",\"job_id\":\"$JOB_ID\",\"lease_id\":$LEASE_ID}"
    
    else
        echo "[worker $WORKER_ID] HANGING on job $JOB_ID lease $LEASE_ID (simulating crash)"
        sleep 35
    fi

    sleep 1
done