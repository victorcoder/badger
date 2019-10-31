!/bin/bash

set -e

# Ensure that we can compile the binary.
pushd badger
go build -v .
popd

# Run the memory intensive tests first.
go test -v --manual=true -run='TestBigKeyValuePairs$'
go test -v --manual=true -run='TestPushValueLogLimit'

# Run the special Truncate test.
rm -rf p
go test -v --manual=true -run='TestTruncateVlogNoClose$' .
truncate --size=4096 p/000001.vlog
go test -v --manual=true -run='TestTruncateVlogNoClose2$' .
go test -v --manual=true -run='TestTruncateVlogNoClose3$' .
rm -rf p

# Then the normal tests.
echo
echo "==> Starting tests with value log mmapped..."
sleep 5
go test -failfast -v --vlog_mmap=true -race ./...

echo
echo "==> Starting tests with value log not mmapped..."
sleep 5
go test -failfast -v --vlog_mmap=false -race ./...
