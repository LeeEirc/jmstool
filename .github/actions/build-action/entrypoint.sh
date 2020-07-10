#!/bin/sh -l

echo "当前目录： ${pwd}"
ls "${pwd}"
echo "Hello $1 $2 $3"
time=$(date)
echo "::set-output name=time::$time"