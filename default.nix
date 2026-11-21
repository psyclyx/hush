let
  npins = import ./npins;

  mkPackages =
    pkgs: lib:
    if builtins.pathExists ./nix/packages then
      lib.packagesFromDirectoryRecursive {
        inherit (pkgs) callPackage;
        directory = ./nix/packages;
      }
    else
      { };

  # Package calls are scoped against `final` so packages under nix/packages
  # can refer to one another. Directory discovery uses `prev.lib` to avoid
  # asking for an attribute of the fixpoint while its overlay keys are still
  # being formed.
  overlay = final: prev: mkPackages final prev.lib;
in
{
  sources ? npins,
  nixpkgs ? sources.nixpkgs,
  pkgs ? import nixpkgs { },
  ...
}:
let
  finalPkgs = pkgs.extend overlay;
in
rec {
  packages = mkPackages finalPkgs pkgs.lib;
  inherit overlay;
  # The module builds hush against the host's own pkgs, so consumers
  # needn't apply the overlay.
  nixosModules.default = ./nix/module.nix;
  shell = finalPkgs.callPackage ./nix/shell.nix { };
  default = packages.hush;
}
