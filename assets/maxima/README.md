# Maxima

Xodus Nexus installs and launches EA games through
[Maxima](https://github.com/ArmchairDevelopers/Maxima), an open-source EA
launcher by ArmchairDevelopers, run as a separate program.

Maxima is licensed under the GNU General Public License v3.0
(`LICENSE.GPL-3.0`). The patches in this directory modify Maxima and are
licensed under GPL-3.0 as well, not under the MIT license of Xodus Nexus.

`maxima-source.lock.json` pins the upstream commit. Upstream does not commit
`Cargo.lock`, so the one Nexus was tested with is kept here. Nexus builds that
commit with this `Cargo.lock` and these patches; together they are the
complete corresponding source.
