#!/usr/bin/env bash
set -e

cd $1

echo -n "Running tests "
function testrun {
    sudo -E bash -c "umask 0; PATH=$PATH go test $@"
}
if [ ! -z "${COVERALLS:-""}" ]; then
    echo "with coverage profile generation..."
    testrun "-race -covermode atomic -coverprofile coverage.out ./..."
else
    echo "without coverage profile generation..."
    testrun "-race ./..."
fi
