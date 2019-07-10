# envdeploy

envdeploy is a web server that runs and provides access to multiple instances of a web application, each instance being separate group of system processes. Think Jupyter notebooks. envdeploy spawns the instance's process in its own cgroup and a separate Linux network namespace. An URL path prefix is reserved for the instance and envdeploy serves as a proxy routing the prefix to the instance's network namespace.

envdeploy can complement a containerization tool such as systemd-nspawn to isolate the instances. envdeploy is responsible for network isolation and lifecycle management, while the other tool provides rest of the isolation by entering new namespaces in the remaining domains. It suffices to use the other tool in the application launch script supplied to envdeploy.

In case of interest, let me know at *nickname*@protonmail.com.
