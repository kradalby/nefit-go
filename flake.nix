{
  description = "Nefit Easy Go library - XMPP client for Bosch/Nefit thermostats";

  inputs = {
    nixpkgs.url = "nixpkgs/nixpkgs-unstable";
    flake-utils.url = "github:numtide/flake-utils";
    flake-checks.url = "github:kradalby/flake-checks";
    flake-checks.inputs.nixpkgs.follows = "nixpkgs";
    flake-checks.inputs.flake-utils.follows = "flake-utils";
  };

  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
      flake-checks,
      ...
    }:
    # Not eachDefaultSystem: nixpkgs does not evaluate on x86_64-darwin.
    flake-utils.lib.eachSystem [ "x86_64-linux" "aarch64-linux" "aarch64-darwin" ] (
      system:
      let
        # The Go dev tools that treefmt drives must be built against the same
        # Go as the module itself. goimports (from `gotools`) ships wrapped
        # with a `go` on PATH; if that `go` is older than the go.mod
        # directive, GOTOOLCHAIN=auto tries to fetch a toolchain from inside
        # the network-less treefmt sandbox and the `formatting` check fails.
        # buildGoLatestModule / go_latest keep this future-proof: bare
        # `pkgs.go` and `pkgs.buildGoModule` may lag behind go.mod, so both
        # must be named explicitly.
        goOverlay = _: prev: {
          gotools = prev.gotools.override {
            buildGoModule = prev.buildGoLatestModule;
            go = prev.go_latest;
          };
          gofumpt = prev.gofumpt.override {
            buildGoModule = prev.buildGoLatestModule;
          };
        };

        pkgs = import nixpkgs {
          inherit system;
          overlays = [ goOverlay ];
        };
        fc = flake-checks.lib;
        common = {
          inherit pkgs;
          root = ./.;
          pname = "nefit-go";
          version = "0.1.0";
          vendorHash = "sha256-9I4lg8EK+EfUtDnaSAUU7snl21i4Za+fw3ql+vNlyx0=";
          # go_latest, not bare `pkgs.go`, which may lag behind go.mod.
          # flake-checks feeds this to
          # `buildGoModule.override { go = goPkg; }`, so this is the single
          # knob that pins every check to the newest Go.
          goPkg = pkgs.go_latest;
        };
      in
      {
        packages.default = fc.goBuild common;

        formatter = fc.formatter common;

        checks = {
          build = fc.goBuild common;
          gotest = fc.goTest common;
          # The race detector needs cgo; stdenv supplies the C compiler.
          gotest-race = fc.goTest (
            common
            // {
              name = "nefit-go-gotest-race";
              goRace = true;
              testEnv = "export CGO_ENABLED=1";
            }
          );
          golangci-lint = fc.goLint common;
          formatting = fc.goFormat common;
          research =
            let
              corpusSource = nixpkgs.lib.fileset.toSource {
                root = ./.;
                fileset = nixpkgs.lib.fileset.unions [
                  ./research
                  ./protocol/testdata/xmpp
                ];
              };
            in
            pkgs.runCommand "nefit-corpus-tests" { nativeBuildInputs = [ pkgs.python3 ]; } ''
              export PYTHONDONTWRITEBYTECODE=1
              python3 -m unittest discover -s ${corpusSource}/research -p 'test_*.py'
              touch "$out"
            '';
        }
        // nixpkgs.lib.optionalAttrs pkgs.stdenv.hostPlatform.isLinux {
          nixos-module = import ./nix/module-check.nix {
            inherit pkgs nixpkgs;
            module = ./nix/module.nix;
          };
        };

        devShells.default = pkgs.mkShell {
          buildInputs = with pkgs; [
            go_latest
            gopls
            gotools
            go-tools
            golangci-lint
            delve
            prek
            nixfmt
            python3
          ];

          shellHook = ''
            echo "Nefit Easy Go development environment"
            echo "Go version: $(go version)"
            echo ""
            echo "CGO is disabled (CGO_ENABLED=0)"
            echo ""
          '';

          # Disable CGO for static builds
          CGO_ENABLED = "0";

          # Go environment
          GOROOT = "${pkgs.go_latest}/share/go";
        };
      }
    )
    // {
      nixosModules.default = import ./nix/module.nix;
    };
}
