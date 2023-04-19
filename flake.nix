{
  description = "Kanonier";
  inputs = {
    nixpkgs.url = "flake:nixpkgs/nixos-22.11";
  };

  outputs = { self, nixpkgs }:
    let pkgs = nixpkgs.legacyPackages.x86_64-linux;
    in {
      devShell.x86_64-linux = pkgs.mkShell {
        nativeBuildInputs = with pkgs; [
          go_1_20
        ];
      };
    };
}
