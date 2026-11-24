package controller

// SetupWithManager registers the fixture controllers.
func (r *WidgetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := ctrl.NewControllerManagedBy(mgr).For(&Widget{}).Named("widget").Complete(r); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).For(&Widget{}).Named("widget-gc").Complete(r)
}
