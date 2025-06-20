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

    # Skip if environment name is empty or null
    if [[ -z "$env_name" || "$env_name" == "null" ]]; then
        continue
    fi

    # Retrieve all containers
    containers=$(curl -s --unix-socket /var/run/docker.sock http://localhost/containers/json?all=1)

    # Filter containers attached to this network
    related_containers=$(echo "$containers" | jq -c "[.[] | select(.Labels.\"com.docker.compose.project\" == \"$env_name\")]")

    # Check whether at least one container is running
    is_running=$(echo "$related_containers" | jq '[.[] | select(.State == "running")] | length > 0')

    # Use the first container to get its working directory
    first_container_id=$(echo "$related_containers" | jq -r '.[0].Id // empty')

    # Default fallback
    url="https://app.${env_name}.test"
    domain="test"
    subdomain="app.${env_name}"

    if [[ -n "$first_container_id" ]]; then
        inspect=$(curl -s --unix-socket /var/run/docker.sock http://localhost/containers/${first_container_id}/json)
        workdir=$(echo "$inspect" | jq -r '.Config.Labels["com.docker.compose.project.working_dir"] // ""')

        if [[ -f "$workdir/.env.roll" ]]; then
            custom_domain=$(grep '^TRAEFIK_DOMAIN=' "$workdir/.env.roll" | cut -d '=' -f2 | tr -d ' ')
            custom_subdomain=$(grep '^TRAEFIK_SUBDOMAIN=' "$workdir/.env.roll" | cut -d '=' -f2 | tr -d ' ')
            
            if [[ -n "$custom_domain" ]]; then
                domain="$custom_domain"
            fi
            if [[ -n "$custom_subdomain" ]]; then
                subdomain="$custom_subdomain"
            fi
            
            url="https://${subdomain}.${domain}"
        fi
    fi

    # Detect additional services by checking running containers for this project
    additional_services="[]"
    if [[ -n "$env_name" ]]; then
        services_json="["
        services_first=1
        
        # Check for ElasticVue container
        elasticvue_running=$(echo "$related_containers" | jq "[.[] | select(.Names[]? | contains(\"elasticvue\")) | select(.State == \"running\")] | length > 0")
        if [[ "$elasticvue_running" == "true" ]]; then
            [[ "$services_first" -eq 0 ]] && services_json+=","
            services_first=0
            services_json+="{\"name\": \"ElasticVue\", \"url\": \"https://elasticvue.${env_name}.${domain}\"}"
        fi
        
        # Check for RabbitMQ container
        rabbitmq_running=$(echo "$related_containers" | jq "[.[] | select(.Names[]? | contains(\"rabbitmq\")) | select(.State == \"running\")] | length > 0")
        if [[ "$rabbitmq_running" == "true" ]]; then
            [[ "$services_first" -eq 0 ]] && services_json+=","
            services_first=0
            services_json+="{\"name\": \"RabbitMQ\", \"url\": \"https://rabbitmq.${env_name}.${domain}\"}"
        fi
        
        # Check for Elasticsearch container
        elasticsearch_running=$(echo "$related_containers" | jq "[.[] | select(.Names[]? | contains(\"elasticsearch\")) | select(.State == \"running\")] | length > 0")
        if [[ "$elasticsearch_running" == "true" ]]; then
            [[ "$services_first" -eq 0 ]] && services_json+=","
            services_first=0
            services_json+="{\"name\": \"Elasticsearch\", \"url\": \"https://elasticsearch.${env_name}.${domain}\"}"
        fi
        
        # Check for Redis Insight container
        redisinsight_running=$(echo "$related_containers" | jq "[.[] | select(.Names[]? | contains(\"redisinsight\")) | select(.State == \"running\")] | length > 0")
        if [[ "$redisinsight_running" == "true" ]]; then
            [[ "$services_first" -eq 0 ]] && services_json+=","
            services_first=0
            services_json+="{\"name\": \"Redis Insight\", \"url\": \"https://redisinsight.${env_name}.${domain}\"}"
        fi
        
        # Check for Varnish container
        varnish_running=$(echo "$related_containers" | jq "[.[] | select(.Names[]? | contains(\"varnish\")) | select(.State == \"running\")] | length > 0")
        if [[ "$varnish_running" == "true" ]]; then
            [[ "$services_first" -eq 0 ]] && services_json+=","
            services_first=0
            services_json+="{\"name\": \"Varnish\", \"url\": \"https://varnish.${env_name}.${domain}\"}"
        fi
        
        services_json+="]"
        additional_services="$services_json"
    fi

    [[ "$first" -eq 0 ]] && echo ","
    first=0

    echo "  { \"name\": \"$env_name\", \"type\": \"$env_type\", \"url\": \"$url\", \"running\": $is_running, \"services\": $additional_services }"
done

echo "]"
