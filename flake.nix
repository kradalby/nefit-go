{
  description = "Nefit Easy Go library - XMPP client for Bosch/Nefit thermostats";

  inputs = {
    nixpkgs.url = "nixpkgs/nixpkgs-unstable";
    flake-utils.url = "github:numtide/flake-utils";
    flake-checks.url = "github:kradalby/flake-checks";
    flake-checks.inputs.nixpkgs.follows = "nixpkgs";
  };

  outputs =
    { self
    , nixpkgs
    , flake-utils
    , flake-checks
    , ...
    }:
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
        fc = flake-checks.lib;
        common = {
          inherit pkgs;
          root = ./.;
          pname = "nefit-go";
          version = "0.1.0";
          vendorHash = "sha256-eBzCJMRygFjzmeg9Wd5EBh1AQ4Z8mhnDilMcXQvE+QA=";
          goPkg = pkgs.go_1_26;
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
            go_1_26
            gopls
            gotools
            go-tools
            golangci-lint
            delve
            prek
            nixpkgs-fmt
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
          GOROOT = "${pkgs.go_1_26}/share/go";
        };
      }
    );
}
