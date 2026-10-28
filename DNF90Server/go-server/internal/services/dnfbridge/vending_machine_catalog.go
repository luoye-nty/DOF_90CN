package dnfbridge

import "fmt"

func (s *Service) currentVendingMachinePVFCatalog() (*currentVendingMachineCatalog, error) {
	if s == nil {
		return nil, errCurrentVendingMachineCatalogUnavailable
	}
	s.vendingMachineCatalogMu.Lock()
	defer s.vendingMachineCatalogMu.Unlock()
	if s.vendingMachineCatalog != nil || s.vendingMachineCatalogLoadErr != nil {
		return s.vendingMachineCatalog, s.vendingMachineCatalogLoadErr
	}
	itemCatalog, err := s.currentPVFItemCatalog()
	if err == nil && itemCatalog != nil {
		s.vendingMachineCatalog, err = newCurrentVendingMachineCatalog(itemCatalog.source)
	}
	if err == nil && s.vendingMachineCatalog == nil {
		err = errCurrentVendingMachineCatalogUnavailable
	}
	if err != nil {
		err = fmt.Errorf("load current vending-machine catalog: %w", err)
	}
	s.vendingMachineCatalogLoadErr = err
	return s.vendingMachineCatalog, s.vendingMachineCatalogLoadErr
}
