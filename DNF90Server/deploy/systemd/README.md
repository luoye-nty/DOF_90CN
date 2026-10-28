# dnfbridge systemd memory guard

Install the drop-in on CentOS 7 after copying the binary and configuration:

```text
mkdir -p /etc/systemd/system/dnfbridge.service.d
cp deploy/systemd/dnfbridge.service.d/memory.conf /etc/systemd/system/dnfbridge.service.d/memory.conf
systemctl daemon-reload
systemctl restart dnfbridge.service
systemctl show dnfbridge.service --property=MemoryLimit,Restart,RestartUSec
```

The drop-in sets `GOMEMLIMIT=2684354560` (2.5 GiB), a 3.2 GiB cgroup limit,
and `Restart=on-failure`. A cgroup memory termination is therefore restarted
by systemd instead of taking down the host. The existing service unit remains
the owner of the executable, user, ports, and database configuration.

To roll back only this guard, remove the drop-in, run `systemctl daemon-reload`,
and restart the service. This does not touch the database or PVF.
