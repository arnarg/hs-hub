{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.hs-hub;
  hs = config.services.headscale;

  inherit (lib)
    mkEnableOption
    mkIf
    mkOption
    types
    ;

  stateDir = "/var/lib/hs-hub";

  magicDnsDomain = lib.optional (
    hs.enable && hs.settings.dns.base_domain != ""
  ) hs.settings.dns.base_domain;

  args = [
    "-socket=${cfg.socket}"
    "-tag=${cfg.tag}"
    "-manifest-port=${toString cfg.manifestPort}"
    "-output=${cfg.outputPath}"
    "-state=${stateDir}/state.json"
    "-interval=${cfg.interval}"
    "-grace=${cfg.grace}"
    "-fetch-timeout=${cfg.fetchTimeout}"
    "-log-level=${cfg.logLevel}"
  ]
  ++ map (d: "-domain=${d}") cfg.domains
  ++ map (d: "-exclude-domain=${d}") (lib.unique (cfg.excludeDomains ++ magicDnsDomain))
  ++ map (n: "-reserved=${n}") cfg.reserved
  ++ lib.optional (cfg.staticRecordsFile != null) "-static-records=${cfg.staticRecordsFile}"
  ++ cfg.extraArgs;
in
{
  options.services.hs-hub = {
    enable = mkEnableOption "hs-hub, which publishes services announced by tailnet nodes as Headscale extra DNS records";

    package = mkOption {
      type = types.package;
      default = pkgs.callPackage ./package.nix { };
      defaultText = lib.literalExpression "pkgs.callPackage ./package.nix { }";
      description = "The hs-hub package to use.";
    };

    socket = mkOption {
      type = types.str;
      default = "/run/tailscale/tailscaled.sock";
      description = "Path to tailscaled's LocalAPI socket.";
    };

    tag = mkOption {
      type = types.str;
      default = "tag:server";
      description = ''
        Tag a node must carry for its manifest to be fetched. The tailnet
        policy must let this host reach `<tag>:<manifestPort>`, otherwise the
        nodes are not visible as peers.
      '';
    };

    manifestPort = mkOption {
      type = types.port;
      default = 7777;
      description = "Port nodes serve `/services.json` on.";
    };

    domains = mkOption {
      type = types.nonEmptyListOf types.str;
      example = [ "example.com" ];
      description = "Published names must be strictly below one of these domains.";
    };

    excludeDomains = mkOption {
      type = types.listOf types.str;
      default = [ ];
      description = ''
        Names inside these domains are never published. When Headscale runs
        on this host its MagicDNS `base_domain` is always excluded so nodes
        cannot shadow MagicDNS names.
      '';
    };

    reserved = mkOption {
      type = types.listOf types.str;
      default = [ ];
      example = lib.literalExpression "lib.attrNames config.services.caddy.virtualHosts";
      description = ''
        Names that are never published. List every name that has a public
        DNS record pointing somewhere else, above all the Headscale server
        itself: if a node claimed it, tailnet clients would lose their
        connection to the control server.
      '';
    };

    staticRecordsFile = mkOption {
      type = types.nullOr types.str;
      default = null;
      example = "/var/lib/headplane/dns-records.json";
      description = ''
        Records file, in Headscale's extra records format, merged into the
        published records, e.g. Headplane's `headscale.dns_records_path`.
        Its names take precedence over names claimed by nodes. It is checked
        for changes every two seconds and must be readable by the hs-hub
        user.
      '';
    };

    interval = mkOption {
      type = types.str;
      default = "30s";
      description = "Time between rounds (Go duration).";
    };

    grace = mkOption {
      type = types.str;
      default = "10m";
      description = "How long names of unreachable or departed nodes are kept (Go duration).";
    };

    fetchTimeout = mkOption {
      type = types.str;
      default = "3s";
      description = "Timeout for a single manifest fetch (Go duration).";
    };

    logLevel = mkOption {
      type = types.enum [
        "debug"
        "info"
        "warn"
        "error"
      ];
      default = "info";
      description = "Log level.";
    };

    outputPath = mkOption {
      type = types.str;
      default = "${stateDir}/records.json";
      readOnly = true;
      description = "The records file, for Headscale's `dns.extra_records_path`.";
    };

    configureHeadscale = mkOption {
      type = types.bool;
      default = true;
      description = "Point the local Headscale's `dns.extra_records_path` at the records file.";
    };

    extraArgs = mkOption {
      type = types.listOf types.str;
      default = [ ];
      description = "Extra command line arguments.";
    };
  };

  config = mkIf cfg.enable {
    users.users.hs-hub = {
      isSystemUser = true;
      group = "hs-hub";
    };
    users.groups.hs-hub = { };

    # Create the directory and an empty records file at boot, so Headscale
    # can start before the hub has completed its first round.
    systemd.tmpfiles.rules = [
      "d ${stateDir} 0755 hs-hub hs-hub -"
      "f ${cfg.outputPath} 0644 hs-hub hs-hub - []"
    ];

    services.headscale.settings.dns.extra_records_path = mkIf (
      cfg.configureHeadscale && hs.enable
    ) cfg.outputPath;

    systemd.services.hs-hub = {
      description = "Publish tailnet services as Headscale extra DNS records";
      wantedBy = [ "multi-user.target" ];
      wants = [ "tailscaled.service" ];
      after = [
        "network-online.target"
        "tailscaled.service"
      ]
      ++ lib.optional hs.enable "headscale.service";
      requires = [ "network-online.target" ];

      serviceConfig = {
        ExecStart = lib.escapeShellArgs ([ (lib.getExe cfg.package) ] ++ args);
        ExecReload = "${pkgs.coreutils}/bin/kill -HUP $MAINPID";
        Restart = "always";
        RestartSec = "5s";

        User = "hs-hub";
        Group = "hs-hub";
        StateDirectory = "hs-hub";
        StateDirectoryMode = "0755";
        UMask = "0022";

        # Hardening
        CapabilityBoundingSet = "";
        LockPersonality = true;
        MemoryDenyWriteExecute = true;
        NoNewPrivileges = true;
        PrivateDevices = true;
        PrivateTmp = true;
        ProtectClock = true;
        ProtectControlGroups = true;
        ProtectHome = true;
        ProtectHostname = true;
        ProtectKernelLogs = true;
        ProtectKernelModules = true;
        ProtectKernelTunables = true;
        ProtectProc = "invisible";
        ProtectSystem = "strict";
        RestrictAddressFamilies = [
          "AF_UNIX"
          "AF_INET"
          "AF_INET6"
        ];
        RestrictNamespaces = true;
        RestrictRealtime = true;
        RestrictSUIDSGID = true;
        SystemCallArchitectures = "native";
        SystemCallFilter = [
          "@system-service"
          "~@privileged"
        ];
      };
    };
  };
}
