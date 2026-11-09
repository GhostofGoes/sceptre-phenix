//go:build e2e

// Package e2e holds end-to-end tests that drive a real phenix binary against a
// real minimega daemon running real QEMU/KVM guests.
//
// The tests are black-box: they run the phenix CLI, call the phenix REST API,
// and check results by talking to minimega's command socket directly, without
// reusing phenix's own minimega client or parsers. They need root, a running
// minimega, Open vSwitch, /dev/kvm, and the guest images named by the E2E_*
// environment variables; see README.md in this directory.
//
// Run them with the e2e build tag, as root, one shard at a time: each shard in
// .github/e2e/shards.json selects its tests by name prefix, and some need more
// environment (TestImagesSmoke needs E2E_SMOKE_IMAGES, the apps, SCORCH and
// domain shards need sceptre-phenix-apps installed and E2E_APPS=true):
//
//	go test -c -tags e2e -o e2e.test ./e2e
//	sudo -E ./e2e.test -test.v -test.run '^TestCore'
//
// Use a dedicated host. The tests use phenix's default store
// (bolt:///etc/phenix/store.bdb) and base directory (/phenix), clear the
// minimega namespaces of their experiments, and expect every QEMU process
// under minimega's base to be theirs.
package e2e
