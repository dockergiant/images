#!/bin/bash
echo "Content-Type: application/json"
echo ""

echo "["

# Retrieve all networks with a dev.roll label
networks=$(curl -s --unix-socket /var/run/docker.sock \
  http://localhost/networks | jq -c '.[] | select(.Labels."dev.roll.environment.name")')

first=1

echo "$networks" | while read -r net; do
    env_name=$(echo "$net" | jq -r '.Labels["dev.roll.environment.name"]')
    env_type=$(echo "$net" | jq -r '.Labels["dev.roll.environment.type"]')

    # Retrieve all containers
    containers=$(curl -s --unix-socket /var/run/docker.sock http://localhost/containers/json?all=1)

    # Filter containers attached to this network
    related_containers=$(echo "$containers" | jq -c "[.[] | select(.Labels.\"com.docker.compose.project\" == \"$env_name\")]")

    # Check whether at least one container is running
    is_running=$(echo "$related_containers" | jq '[.[] | select(.State == "running")] | length > 0')

    # Use the first container to get its working directory
    first_container_id=$(echo "$related_containers" | jq -r '.[0].Id // empty')

    # Default fallback
    url="https://${env_name}.roll.test"

    if [[ -n "$first_container_id" ]]; then
        inspect=$(curl -s --unix-socket /var/run/docker.sock http://localhost/containers/${first_container_id}/json)
        workdir=$(echo "$inspect" | jq -r '.Config.Labels["com.docker.compose.project.working_dir"] // ""')

        if [[ -f "$workdir/.env.roll" ]]; then
            domain=$(grep '^TRAEFIK_DOMAIN=' "$workdir/.env.roll" | cut -d '=' -f2 | tr -d ' ')
            subdomain=$(grep '^TRAEFIK_SUBDOMAIN=' "$workdir/.env.roll" | cut -d '=' -f2 | tr -d ' ')
            url="https://${subdomain}.${domain}"
        fi
    fi

    [[ "$first" -eq 0 ]] && echo ","
    first=0

    echo "  { \"name\": \"$env_name\", \"type\": \"$env_type\", \"url\": \"$url\", \"running\": $is_running }"
done

echo "]"
