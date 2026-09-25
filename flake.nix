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
    # Not eachDefaultSystem: nixpkgs 26.11 dropped x86_64-darwin and throws on eval.
    flake-utils.lib.eachSystem [ "x86_64-linux" "aarch64-linux" "aarch64-darwin" ] (
      system:
      let
        # The Go dev tools that treefmt drives must be built against the same
        # Go as the module itself. goimports (from `gotools`) ships wrapped
        # with a `go` on PATH; if that `go` is older than the go.mod
        # directive, GOTOOLCHAIN=auto tries to fetch a toolchain from inside
        # the network-less treefmt sandbox and the `formatting` check fails.
        # buildGoLatestModule / go_latest keep this future-proof: bare
        # `pkgs.go` and `pkgs.buildGoModule` still resolve to the previous
        # stable (1.26), so both must be named explicitly.
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
          vendorHash = "sha256-+BIvF8EX10Nefc93JvaoSSd5BHN0rUFoI3+2bLVYTzw=";
          # go_latest, not bare `pkgs.go`: the latter still resolves to the
          # previous stable (1.26) in nixpkgs. flake-checks feeds this to
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
          golangci-lint = fc.goLint common;
          formatting = fc.goFormat common;
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
    );
}
