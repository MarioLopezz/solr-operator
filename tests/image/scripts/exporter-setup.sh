#!/bin/bash
max_try=${MAX_TRY:-12}
wait_seconds=${WAIT_SECONDS:-10}
if ! /opt/docker-solr/scripts/wait-for-solr.sh --max-attempts "$max_try" --wait-seconds "$wait_seconds"; then
    exit 1
fi

/opt/solr-$SOLR_VERSION/contrib/prometheus-exporter/bin/solr-exporter -p 9983 -z $ZK_HOST -f /opt/solr-$SOLR_VERSION/contrib/prometheus-exporter/conf/solr-exporter-config.xml