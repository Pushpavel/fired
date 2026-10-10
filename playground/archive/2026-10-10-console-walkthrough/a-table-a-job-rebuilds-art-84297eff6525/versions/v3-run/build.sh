#!/bin/sh
set -e
mkdir -p data
awk -v seed="$(date +%s)" 'BEGIN {
  srand(seed); print "region,orders,revenue"
  split("North South East West Central", r, " ")
  for (i = 1; i <= 5; i++) { o = int(50 + rand() * 450); printf "%s,%d,%.2f\n", r[i], o, o * (18 + rand() * 12) }
}' > data/sales.csv
echo "wrote $(wc -l < data/sales.csv) lines to data/sales.csv"
