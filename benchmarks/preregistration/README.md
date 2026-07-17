# Pre-registrations

One frozen pre-registration per capability card, written **before** any
measured run (§3.0). Copy `TEMPLATE.md` to `<capability>-<date>.md`, fill
every field, and freeze it (fill the §12 freeze line + manifest hash).

- The primary endpoint, MPIE, design, cache regime, power, analysis, and
  exclusion rules are all fixed here before results exist.
- A frozen pre-registration is hashed into the drift manifest (§3.4); the
  website claim-manifest test (§4.5) checks the card links to it.
- Changing a pre-registration after seeing results invalidates the card
  (R16). Start a new dated file instead.

Phase 0 ships only the template. The first real pre-registration
(`tool-defs-trim-<date>.md`) is authored at the **Phase 0a gate**, after
the pilot supplies the per-pair variance the power calc needs.
