output "public_ip" {
  description = "Point the site's A record (and grafana.<domain>) here"
  value       = oci_core_instance.ridewatch.public_ip
}
