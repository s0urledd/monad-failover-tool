# Verifying reboot safety on a disposable VM

The test suite proves the masking logic with mocks: it checks that the units are
masked before the swap, that a mask which does not take effect stops the run, and
that units masked beforehand stay masked. Mocks cannot prove what a real kernel
and a real systemd do across a reboot. This is the procedure for that, and it
needs a throwaway VM. Do not run it on a machine holding real validator keys.

## Setup

1. Bring up a disposable Ubuntu VM and install a Monad full node on it, following
   the official full node installation guide. Let it sync, or stop after the
   services exist if you only want to test the swap mechanics.
2. Generate a throwaway keypair to use as the "validator" being migrated. Never
   use a real validator key for this.
3. Install the tool as in the README.

## The test

1. Start a live run and answer the prompts until you reach the `STOPPED` gate.
2. Before typing `STOPPED`, open a second shell and confirm the units are enabled:

   ```bash
   systemctl is-enabled monad-bft monad-execution monad-rpc
   ```

3. Type `STOPPED` and confirm the cutover, then immediately interrupt the run
   part-way through the swap. The window is small, so the reliable way is to add
   a temporary `sleep 30` between the first and second `place_staged` call in
   your copy of the script, and kill the run during that sleep.
4. With the run killed mid-swap, confirm the units are masked:

   ```bash
   systemctl is-enabled monad-bft monad-execution monad-rpc   # expect: masked
   ```

5. Reboot the VM.
6. After it comes back, confirm the services did **not** start:

   ```bash
   systemctl is-active monad-bft monad-execution monad-rpc    # expect: inactive
   journalctl -u monad-bft -b | tail
   ```

   This is the property being tested. Without the mask the units would have
   started here with one new key and one old one.

7. Finish the migration:

   ```bash
   monad-failover --resume
   ```

8. Confirm the units are unmasked and active, and that the identity is
   consistent (both keys and `node.toml` are the new ones).

## What a pass looks like

- Units report `masked` while the swap is unfinished.
- Nothing starts after the reboot.
- `--resume` completes the swap, unmasks, starts the services, and the node comes
  up with a single consistent identity.

Record the systemd output from steps 4 and 6; that is the evidence. A green test
suite is not a substitute for it.
