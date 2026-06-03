<?php
/*
 * idp-config-override.php — pinned baseurlpath + entityid for the dev
 * SAML IdP (kristophjunge/test-saml-idp). Fixes the dynamic-entityID
 * trap caught in Issue 5 of the 2026-06-02 teams test findings:
 *
 *   SimpleSAMLphp's default IDP-hosted metadata uses entityID
 *   `__DYNAMIC:1__` and host `__DEFAULT__`, so the entityID it
 *   advertises depends on whichever hostname the request was made to.
 *   From the SP (inside the docker network) that's `idp:8080`; from a
 *   WSL host browser that's `localhost:8088`. The mismatch produces
 *   either an unreachable redirect or a 403 at the ACS.
 *
 *   Pinning baseurlpath to a single absolute URL makes the IdP
 *   advertise the same entityID + SSO URL no matter who fetches the
 *   metadata.
 *
 * This file is mounted over /var/www/simplesamlphp/config/config.php
 * via docker-compose. The `127.0.0.1 idp` extra_hosts entry on the
 * `org` service lets the SP also reach `idp` at the same address from
 * inside the network.
 *
 * DEV ONLY. The pinned URL is the host-reachable
 * `http://localhost:8088/simplesaml/` and is suitable only for the
 * compose dev stack — never use this file in a real deployment.
 */

$config = require '/var/simplesamlphp/config/config.php.dist';
$config['baseurlpath'] = 'http://localhost:8088/simplesaml/';
$config['secretsalt'] = 'observer-org-dev-fixed-salt-not-a-secret';
$config['auth.adminpassword'] = 'observer-org-dev-admin-pass';
$config['admin.protectindexpage'] = false;
$config['admin.protectmetadata'] = false;
$config['enable.saml20-idp'] = true;
return $config;
