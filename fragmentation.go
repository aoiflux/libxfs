package libxfs

// AnalyzeInodeFragmentation analyzes the data-fork extent layout of an inode.
func (v *Volume) AnalyzeInodeFragmentation(inodeNumber uint64) (FragmentationReport, error) {
	inode, err := v.OpenInode(inodeNumber)
	if err != nil {
		return FragmentationReport{}, err
	}

	report := FragmentationReport{
		InodeNumber:     inodeNumber,
		Size:            inode.Size,
		DataExtentCount: len(inode.DataExtents),
	}

	allocated := make([]Extent, 0, len(inode.DataExtents))
	for _, extent := range inode.DataExtents {
		if extent.NumberOfBlocks == 0 {
			continue
		}
		if (extent.RangeFlags & ExtentFlagSparse) != 0 {
			report.SparseExtentCount++
			report.HasLogicalHoles = true
			continue
		}
		allocated = append(allocated, extent)
	}

	report.AllocatedExtentCount = len(allocated)
	if len(allocated) == 0 {
		report.HasAnyFragmentationOrHoles = report.HasLogicalHoles
		return report, nil
	}

	runs := 1
	for i := 1; i < len(allocated); i++ {
		prev := allocated[i-1]
		curr := allocated[i]

		expectedLogical := prev.LogicalBlockNumber + uint64(prev.NumberOfBlocks)
		if curr.LogicalBlockNumber > expectedLogical {
			report.HasLogicalHoles = true
		}

		expectedPhysical := prev.PhysicalBlockNumber + uint64(prev.NumberOfBlocks)
		if curr.PhysicalBlockNumber != expectedPhysical || curr.LogicalBlockNumber != expectedLogical {
			runs++
		}
	}

	report.PhysicalFragmentRuns = runs
	report.HasPhysicalFragmentation = runs > 1 || len(allocated) > 1
	report.HasAnyFragmentationOrHoles = report.HasPhysicalFragmentation || report.HasLogicalHoles
	return report, nil
}

// AnalyzeInodeFragmentationByPath resolves a file path then analyzes fragmentation.
func (v *Volume) AnalyzeInodeFragmentationByPath(path string) (FragmentationReport, error) {
	inodeNumber, err := v.ResolveInodeByPath(path)
	if err != nil {
		return FragmentationReport{}, err
	}
	return v.AnalyzeInodeFragmentation(inodeNumber)
}
