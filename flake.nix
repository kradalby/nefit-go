{
  description = "Nefit Easy Go library - XMPP client for Bosch/Nefit thermostats";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
      in
      {
        devShells.default = pkgs.mkShell {
          buildInputs = with pkgs; [
            go_1_25
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
          GOROOT = "${pkgs.go_1_25}/share/go";
        };

        # Package definition (for building)
        packages.default = pkgs.buildGoModule {
          pname = "nefit-go";
          version = "0.1.0";
          src = ./.;

          vendorHash = null; # Will be updated after go mod vendor

          env = {
            CGO_ENABLED = "0";
          };

          ldflags = [
            "-s"
            "-w"
            "-extldflags=-static"
          ];

          meta = with pkgs.lib; {
            description = "Go library for Nefit Easy smart thermostats";
            homepage = "https://github.com/kradalby/nefit-go";
            license = licenses.mit;
            maintainers = [ ];
          };
        };
      }
    );
}
