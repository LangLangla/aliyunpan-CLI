package syncdrive

import (
	"context"
	"fmt"
	mapset "github.com/deckarep/golang-set"
	"github.com/tickstep/aliyunpan-api/aliyunpan"
	"github.com/tickstep/aliyunpan/internal/localfile"
	"github.com/tickstep/aliyunpan/internal/waitgroup"
	"github.com/tickstep/aliyunpan/library/collection"
	"github.com/tickstep/library-go/logger"
	"path"
	"strings"
	"sync"
	"time"
)

type (
	FileActionTaskList []*FileActionTask

	FileActionTaskManager struct {
		mutex             *sync.Mutex
		folderCreateMutex *sync.Mutex

		task       *SyncTask
		wg         *waitgroup.WaitGroup
		ctx        context.Context
		cancelFunc context.CancelFunc

		fileInProcessQueue   *collection.Queue
		fileDownloadParallel int
		fileUploadParallel   int

		fileDownloadBlockSize int64
		fileUploadBlockSize   int64

		maxDownloadRate int64 // 限制最大下载速度
		maxUploadRate   int64 // 限制最大上传速度

		useInternalUrl bool

		localFolderModifyCount int // 本地文件扫描变更记录次数，作为后续文件对比进程的参考以节省CPU资源
		panFolderModifyCount   int // 云盘文件扫描变更记录次数，作为后续文件对比进程的参考以节省CPU资源
		syncActionModifyCount  int // 文件对比进程检测的文件上传下载删除变更记录次数，作为后续文件上传下载处理进程的参考以节省CPU资源
		resourceModifyMutex    *sync.Mutex
	}

	localFileSet struct {
		items           LocalFileList
		localFolderPath string
	}
	panFileSet struct {
		items         PanFileList
		panFolderPath string
	}
)

func NewFileActionTaskManager(task *SyncTask, maxDownloadRate, maxUploadRate int64) *FileActionTaskManager {
	return &FileActionTaskManager{
		mutex:             &sync.Mutex{},
		folderCreateMutex: &sync.Mutex{},
		task:              task,

		fileInProcessQueue:   collection.NewFifoQueue(),
		fileDownloadParallel: task.fileDownloadParallel,
		fileUploadParallel:   task.fileUploadParallel,

		fileDownloadBlockSize: task.fileDownloadBlockSize,
		fileUploadBlockSize:   task.fileUploadBlockSize,
		useInternalUrl:        task.useInternalUrl,

		maxDownloadRate: maxDownloadRate,
		maxUploadRate:   maxUploadRate,

		localFolderModifyCount: 1,
		panFolderModifyCount:   1,
		syncActionModifyCount:  1,
		resourceModifyMutex:    &sync.Mutex{},
	}
}

func (f *FileActionTaskManager) AddLocalFolderModifyCount() {
	f.resourceModifyMutex.Lock()
	defer f.resourceModifyMutex.Unlock()
	f.localFolderModifyCount += 1
}

func (f *FileActionTaskManager) MinusLocalFolderModifyCount() {
	f.resourceModifyMutex.Lock()
	defer f.resourceModifyMutex.Unlock()
	f.localFolderModifyCount -= 1
	if f.localFolderModifyCount < 0 {
		f.localFolderModifyCount = 0
	}
}

func (f *FileActionTaskManager) getLocalFolderModifyCount() int {
	f.resourceModifyMutex.Lock()
	defer f.resourceModifyMutex.Unlock()
	return f.localFolderModifyCount
}

func (f *FileActionTaskManager) AddPanFolderModifyCount() {
	f.resourceModifyMutex.Lock()
	defer f.resourceModifyMutex.Unlock()
	f.panFolderModifyCount += 1
}

func (f *FileActionTaskManager) MinusPanFolderModifyCount() {
	f.resourceModifyMutex.Lock()
	defer f.resourceModifyMutex.Unlock()
	f.panFolderModifyCount -= 1
	if f.panFolderModifyCount < 0 {
		f.panFolderModifyCount = 0
	}
}

func (f *FileActionTaskManager) getPanFolderModifyCount() int {
	f.resourceModifyMutex.Lock()
	defer f.resourceModifyMutex.Unlock()
	return f.panFolderModifyCount
}

func (f *FileActionTaskManager) AddSyncActionModifyCount() {
	f.resourceModifyMutex.Lock()
	defer f.resourceModifyMutex.Unlock()
	f.syncActionModifyCount += 1
}

func (f *FileActionTaskManager) MinusSyncActionModifyCount() {
	f.resourceModifyMutex.Lock()
	defer f.resourceModifyMutex.Unlock()
	f.syncActionModifyCount -= 1
	if f.syncActionModifyCount < 0 {
		f.syncActionModifyCount = 0
	}
}

func (f *FileActionTaskManager) getSyncActionModifyCount() int {
	f.resourceModifyMutex.Lock()
	defer f.resourceModifyMutex.Unlock()
	return f.syncActionModifyCount
}

// Start 启动文件动作任务管理进程
// 通过对本地数据库的对比，决策对文件进行下载、上传、删除等动作
func (f *FileActionTaskManager) Start() error {
	if f.ctx != nil {
		return fmt.Errorf("task have starting")
	}
	f.wg = waitgroup.NewWaitGroup(0)

	var cancel context.CancelFunc
	f.ctx, cancel = context.WithCancel(context.Background())
	f.cancelFunc = cancel

	go f.doLocalFileDiffRoutine(f.ctx)
	go f.doPanFileDiffRoutine(f.ctx)
	go f.fileActionTaskExecutor(f.ctx)
	return nil
}

func (f *FileActionTaskManager) Stop() error {
	if f.ctx == nil {
		return nil
	}
	// cancel all sub task & process
	f.cancelFunc()

	// wait for finished
	f.wg.Wait()

	f.ctx = nil
	f.cancelFunc = nil

	return nil
}

// getPanPathFromLocalPath 通过本地文件路径获取网盘文件的对应路径
func (f *FileActionTaskManager) getPanPathFromLocalPath(localPath string) string {
	localPath = strings.ReplaceAll(localPath, "\\", "/")
	localRootPath := strings.ReplaceAll(f.task.LocalFolderPath, "\\", "/")

	relativePath := strings.TrimPrefix(localPath, localRootPath)
	return path.Join(path.Clean(f.task.PanFolderPath), relativePath)
}

// getLocalPathFromPanPath 通过网盘文件路径获取对应的本地文件的对应路径
func (f *FileActionTaskManager) getLocalPathFromPanPath(panPath string) string {
	panPath = strings.ReplaceAll(panPath, "\\", "/")
	panRootPath := strings.ReplaceAll(f.task.PanFolderPath, "\\", "/")

	relativePath := strings.TrimPrefix(panPath, panRootPath)
	return path.Join(path.Clean(f.task.LocalFolderPath), relativePath)
}

// doLocalFileDiffRoutine 对比网盘文件和本地文件信息，差异化上传或者下载文件
func (f *FileActionTaskManager) doLocalFileDiffRoutine(ctx context.Context) {
	localFolderQueue := collection.NewFifoQueue()
	var localRootFolder *LocalFileItem
	var er error

	f.wg.AddDelta()
	defer f.wg.Done()
	for {
		select {
		case <-ctx.Done():
			// cancel routine & done
			logger.Verboseln("file diff routine done")
			return
		default:
			if localRootFolder == nil {
				localRootFolder, er = f.task.localFileDb.Get(f.task.LocalFolderPath)
				if er == nil {
					localFolderQueue.Push(localRootFolder)
				} else {
					time.Sleep(1 * time.Second)
					continue
				}
			}
			// check need to do the loop or to wait
			if f.getLocalFolderModifyCount() <= 0 {
				time.Sleep(1 * time.Second)
				continue
			}

			localFiles := LocalFileList{}
			panFiles := PanFileList{}
			var err error
			var objLocal interface{}

			objLocal = localFolderQueue.Pop()
			if objLocal == nil {
				// restart over & begin goto next term
				localFolderQueue.Push(localRootFolder)
				f.MinusLocalFolderModifyCount()
				time.Sleep(3 * time.Second)
				continue
			}
			localItem := objLocal.(*LocalFileItem)
			localFiles, err = f.task.localFileDb.GetFileList(localItem.Path)
			if err != nil {
				localFiles = LocalFileList{}
			}
			panFiles, err = f.task.panFileDb.GetFileList(f.getPanPathFromLocalPath(localItem.Path))
			if err != nil {
				panFiles = PanFileList{}
			}
			f.doFileDiffRoutine(panFiles, localFiles, nil, localFolderQueue)
		}
	}
}

// doPanFileDiffRoutine 对比网盘文件和本地文件信息，差异化上传或者下载文件
func (f *FileActionTaskManager) doPanFileDiffRoutine(ctx context.Context) {
	panFolderQueue := collection.NewFifoQueue()
	var panRootFolder *PanFileItem
	var er error

	f.wg.AddDelta()
	defer f.wg.Done()
	for {
		select {
		case <-ctx.Done():
			// cancel routine & done
			logger.Verboseln("file diff routine done")
			return
		default:
			if panRootFolder == nil {
				panRootFolder, er = f.task.panFileDb.Get(f.task.PanFolderPath)
				if er == nil {
					panFolderQueue.Push(panRootFolder)
				} else {
					time.Sleep(1 * time.Second)
					continue
				}
			}
			if f.getPanFolderModifyCount() <= 0 {
				time.Sleep(1 * time.Second)
				continue
			}

			localFiles := LocalFileList{}
			panFiles := PanFileList{}
			var err error
			var objPan interface{}

			objPan = panFolderQueue.Pop()
			if objPan == nil {
				// restart over
				panFolderQueue.Push(panRootFolder)
				f.MinusPanFolderModifyCount()
				time.Sleep(3 * time.Second)
				continue
			}
			panItem := objPan.(*PanFileItem)
			panFiles, err = f.task.panFileDb.GetFileList(panItem.Path)
			if err != nil {
				panFiles = PanFileList{}
			}
			localFiles, err = f.task.localFileDb.GetFileList(f.getLocalPathFromPanPath(panItem.Path))
			if err != nil {
				localFiles = LocalFileList{}
			}
			f.doFileDiffRoutine(panFiles, localFiles, panFolderQueue, nil)
			time.Sleep(500 * time.Millisecond)
		}
	}
}

func (f *FileActionTaskManager) doFileDiffRoutine(panFiles PanFileList, localFiles LocalFileList, panFolderQueue *collection.Queue, localFolderQueue *collection.Queue) {
	// empty loop
	if len(panFiles) == 0 && len(localFiles) == 0 {
		time.Sleep(100 * time.Millisecond)
		return
	}

	localFilesSet := &localFileSet{
		items:           localFiles,
		localFolderPath: f.task.LocalFolderPath,
	}
	panFilesSet := &panFileSet{
		items:         panFiles,
		panFolderPath: f.task.PanFolderPath,
	}
	localFilesNeedToUpload := localFilesSet.Difference(panFilesSet)
	panFilesNeedToDownload := panFilesSet.Difference(localFilesSet)
	localFilesNeedToCheck, panFilesNeedToCheck := localFilesSet.Intersection(panFilesSet)

	// download file from pan drive
	if panFilesNeedToDownload != nil {
		for _, file := range panFilesNeedToDownload {
			if file.ScanStatus == ScanStatusNormal { // 下载文件
				if f.task.Mode == DownloadOnly || f.task.Mode == SyncTwoWay {
					if file.IsFolder() {
						if panFolderQueue != nil {
							panFolderQueue.PushUnique(file)
						}
						continue
					}
					fileActionTask := &FileActionTask{
						syncItem: &SyncFileItem{
							Action:            SyncFileActionDownload,
							Status:            SyncFileStatusCreate,
							LocalFile:         nil,
							PanFile:           file,
							StatusUpdateTime:  "",
							PanFolderPath:     f.task.PanFolderPath,
							LocalFolderPath:   f.task.LocalFolderPath,
							DriveId:           f.task.DriveId,
							DownloadBlockSize: f.fileDownloadBlockSize,
							UploadBlockSize:   f.fileUploadBlockSize,
							UseInternalUrl:    f.useInternalUrl,
						},
					}
					f.addToSyncDb(fileActionTask)
				}
			} else if file.ScanStatus == ScanStatusDiscard { // 删除对应本地文件（文件夹）
				if f.task.Mode == DownloadOnly || f.task.Mode == SyncTwoWay {
					fileActionTask := &FileActionTask{
						syncItem: &SyncFileItem{
							Action:            SyncFileActionDeleteLocal,
							Status:            SyncFileStatusCreate,
							LocalFile:         nil,
							PanFile:           file,
							StatusUpdateTime:  "",
							PanFolderPath:     f.task.PanFolderPath,
							LocalFolderPath:   f.task.LocalFolderPath,
							DriveId:           f.task.DriveId,
							DownloadBlockSize: f.fileDownloadBlockSize,
							UploadBlockSize:   f.fileUploadBlockSize,
							UseInternalUrl:    f.useInternalUrl,
						},
					}
					f.addToSyncDb(fileActionTask)
				} else if f.task.Mode == UploadOnly {
					// 删除无用记录
					f.task.panFileDb.Delete(file.Path)
				}
			}
		}
	}

	// upload file to pan drive
	if localFilesNeedToUpload != nil {
		for _, file := range localFilesNeedToUpload {
			if file.ScanStatus == ScanStatusNormal { // 上传文件到云盘
				if f.task.Mode == UploadOnly || f.task.Mode == SyncTwoWay {
					if file.IsFolder() {
						if localFolderQueue != nil {
							localFolderQueue.PushUnique(file)
						}
						continue
					}
					fileActionTask := &FileActionTask{
						syncItem: &SyncFileItem{
							Action:            SyncFileActionUpload,
							Status:            SyncFileStatusCreate,
							LocalFile:         file,
							PanFile:           nil,
							StatusUpdateTime:  "",
							PanFolderPath:     f.task.PanFolderPath,
							LocalFolderPath:   f.task.LocalFolderPath,
							DriveId:           f.task.DriveId,
							DownloadBlockSize: f.fileDownloadBlockSize,
							UploadBlockSize:   f.fileUploadBlockSize,
							UseInternalUrl:    f.useInternalUrl,
						},
					}
					f.addToSyncDb(fileActionTask)
				}
			} else if file.ScanStatus == ScanStatusDiscard { // 删除对应云盘文件（文件夹）
				if f.task.Mode == UploadOnly || f.task.Mode == SyncTwoWay {
					fileActionTask := &FileActionTask{
						syncItem: &SyncFileItem{
							Action:            SyncFileActionDeletePan,
							Status:            SyncFileStatusCreate,
							LocalFile:         file,
							PanFile:           nil,
							StatusUpdateTime:  "",
							PanFolderPath:     f.task.PanFolderPath,
							LocalFolderPath:   f.task.LocalFolderPath,
							DriveId:           f.task.DriveId,
							DownloadBlockSize: f.fileDownloadBlockSize,
							UploadBlockSize:   f.fileUploadBlockSize,
							UseInternalUrl:    f.useInternalUrl,
						},
					}
					f.addToSyncDb(fileActionTask)
				} else if f.task.Mode == DownloadOnly {
					// 删除无用记录
					f.task.localFileDb.Delete(file.Path)
				}
			}
		}
	}

	// compare file to decide download / upload / delete
	for idx, _ := range localFilesNeedToCheck {
		localFile := localFilesNeedToCheck[idx]
		panFile := panFilesNeedToCheck[idx]

		//
		// do delete local / pan file check
		//
		if localFile.ScanStatus == ScanStatusDiscard && panFile.ScanStatus == ScanStatusDiscard {
			// 清除过期数据项
			f.task.localFileDb.Delete(localFile.Path)
			f.task.panFileDb.Delete(panFile.Path)
			continue
		}
		if localFile.ScanStatus == ScanStatusDiscard && panFile.ScanStatus == ScanStatusNormal && localFile.Sha1Hash == panFile.Sha1Hash {
			if f.task.Mode == UploadOnly || f.task.Mode == SyncTwoWay {
				// 删除对应的云盘文件
				deletePanFile := &FileActionTask{
					syncItem: &SyncFileItem{
						Action:            SyncFileActionDeletePan,
						Status:            SyncFileStatusCreate,
						LocalFile:         localFile,
						PanFile:           panFile,
						StatusUpdateTime:  "",
						PanFolderPath:     f.task.PanFolderPath,
						LocalFolderPath:   f.task.LocalFolderPath,
						DriveId:           f.task.DriveId,
						DownloadBlockSize: f.fileDownloadBlockSize,
						UploadBlockSize:   f.fileUploadBlockSize,
						UseInternalUrl:    f.useInternalUrl,
					},
				}
				f.addToSyncDb(deletePanFile)
			} else if f.task.Mode == DownloadOnly {
				// 删除无用记录
				f.task.localFileDb.Delete(localFile.Path)
			}
			continue
		}
		if panFile.ScanStatus == ScanStatusDiscard && localFile.ScanStatus == ScanStatusNormal && localFile.Sha1Hash == panFile.Sha1Hash {
			if f.task.Mode == DownloadOnly || f.task.Mode == SyncTwoWay {
				// 删除对应的本地文件
				deletePanFile := &FileActionTask{
					syncItem: &SyncFileItem{
						Action:            SyncFileActionDeleteLocal,
						Status:            SyncFileStatusCreate,
						LocalFile:         localFile,
						PanFile:           panFile,
						StatusUpdateTime:  "",
						PanFolderPath:     f.task.PanFolderPath,
						LocalFolderPath:   f.task.LocalFolderPath,
						DriveId:           f.task.DriveId,
						DownloadBlockSize: f.fileDownloadBlockSize,
						UploadBlockSize:   f.fileUploadBlockSize,
						UseInternalUrl:    f.useInternalUrl,
					},
				}
				f.addToSyncDb(deletePanFile)
			} else if f.task.Mode == UploadOnly {
				// 删除无用记录
				f.task.panFileDb.Delete(panFile.Path)
			}
			continue
		}

		//
		// do download / upload check
		//
		if localFile.IsFolder() {
			if localFolderQueue != nil {
				localFolderQueue.PushUnique(localFile)
			}
			if panFolderQueue != nil {
				panFolderQueue.PushUnique(panFile)
			}
			continue
		}

		if localFile.Sha1Hash == "" {
			// calc sha1
			if localFile.FileSize == 0 {
				localFile.Sha1Hash = aliyunpan.DefaultZeroSizeFileContentHash
			} else {
				fileSum := localfile.NewLocalFileEntity(localFile.Path)
				err := fileSum.OpenPath()
				if err != nil {
					logger.Verbosef("文件不可读, 错误信息: %s, 跳过...\n", err)
					continue
				}
				fileSum.Sum(localfile.CHECKSUM_SHA1) // block operation
				localFile.Sha1Hash = fileSum.SHA1
				fileSum.Close()
			}

			// save sha1
			f.task.localFileDb.Update(localFile)
		}

		if strings.ToLower(panFile.Sha1Hash) == strings.ToLower(localFile.Sha1Hash) {
			// do nothing
			logger.Verboseln("file is the same, no need to update file: ", localFile.Path)
			continue
		}

		// 本地文件和云盘文件SHA1不一样
		// 不同模式同步策略不一样
		if f.task.Mode == UploadOnly {
			uploadLocalFile := &FileActionTask{
				syncItem: &SyncFileItem{
					Action:            SyncFileActionUpload,
					Status:            SyncFileStatusCreate,
					LocalFile:         localFile,
					PanFile:           nil,
					StatusUpdateTime:  "",
					PanFolderPath:     f.task.PanFolderPath,
					LocalFolderPath:   f.task.LocalFolderPath,
					DriveId:           f.task.DriveId,
					DownloadBlockSize: f.fileDownloadBlockSize,
					UploadBlockSize:   f.fileUploadBlockSize,
					UseInternalUrl:    f.useInternalUrl,
				},
			}
			f.addToSyncDb(uploadLocalFile)
		} else if f.task.Mode == DownloadOnly {
			downloadPanFile := &FileActionTask{
				syncItem: &SyncFileItem{
					Action:            SyncFileActionDownload,
					Status:            SyncFileStatusCreate,
					LocalFile:         nil,
					PanFile:           panFile,
					StatusUpdateTime:  "",
					PanFolderPath:     f.task.PanFolderPath,
					LocalFolderPath:   f.task.LocalFolderPath,
					DriveId:           f.task.DriveId,
					DownloadBlockSize: f.fileDownloadBlockSize,
					UploadBlockSize:   f.fileUploadBlockSize,
					UseInternalUrl:    f.useInternalUrl,
				},
			}
			f.addToSyncDb(downloadPanFile)
		} else if f.task.Mode == SyncTwoWay {
			if localFile.UpdateTimeUnix() > panFile.UpdateTimeUnix() { // upload file
				uploadLocalFile := &FileActionTask{
					syncItem: &SyncFileItem{
						Action:            SyncFileActionUpload,
						Status:            SyncFileStatusCreate,
						LocalFile:         localFile,
						PanFile:           nil,
						StatusUpdateTime:  "",
						PanFolderPath:     f.task.PanFolderPath,
						LocalFolderPath:   f.task.LocalFolderPath,
						DriveId:           f.task.DriveId,
						DownloadBlockSize: f.fileDownloadBlockSize,
						UploadBlockSize:   f.fileUploadBlockSize,
						UseInternalUrl:    f.useInternalUrl,
					},
				}
				f.addToSyncDb(uploadLocalFile)
			} else if localFile.UpdateTimeUnix() < panFile.UpdateTimeUnix() { // download file
				downloadPanFile := &FileActionTask{
					syncItem: &SyncFileItem{
						Action:            SyncFileActionDownload,
						Status:            SyncFileStatusCreate,
						LocalFile:         nil,
						PanFile:           panFile,
						StatusUpdateTime:  "",
						PanFolderPath:     f.task.PanFolderPath,
						LocalFolderPath:   f.task.LocalFolderPath,
						DriveId:           f.task.DriveId,
						DownloadBlockSize: f.fileDownloadBlockSize,
						UploadBlockSize:   f.fileUploadBlockSize,
						UseInternalUrl:    f.useInternalUrl,
					},
				}
				f.addToSyncDb(downloadPanFile)
			}
		}
	}
}

func (f *FileActionTaskManager) addToSyncDb(fileTask *FileActionTask) {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	// check sync db
	if itemInDb, e := f.task.syncFileDb.Get(fileTask.syncItem.Id()); e == nil && itemInDb != nil {
		if itemInDb.Status == SyncFileStatusCreate || itemInDb.Status == SyncFileStatusDownloading || itemInDb.Status == SyncFileStatusUploading {
			return
		}
		if itemInDb.Status == SyncFileStatusSuccess {
			if (time.Now().Unix() - itemInDb.StatusUpdateTimeUnix()) < TimeSecondsOf5Minute {
				// 少于5分钟，不同步，减少同步频次
				return
			}
		}
		if itemInDb.Status == SyncFileStatusIllegal {
			if (time.Now().Unix() - itemInDb.StatusUpdateTimeUnix()) < TimeSecondsOf60Minute {
				// 非法文件，少于60分钟，不同步，减少同步频次
				return
			}
		}
		if itemInDb.Status == SyncFileStatusNotExisted {
			if itemInDb.Action == SyncFileActionDownload {
				if itemInDb.PanFile.UpdatedAt == fileTask.syncItem.PanFile.UpdatedAt {
					return
				}
			} else if itemInDb.Action == SyncFileActionUpload {
				if itemInDb.LocalFile.UpdatedAt == fileTask.syncItem.LocalFile.UpdatedAt {
					return
				}
			}
		}
	}

	// 进入任务队列
	f.task.syncFileDb.Add(fileTask.syncItem)

	// label file action modify
	f.AddSyncActionModifyCount()
}

func (f *FileActionTaskManager) getFromSyncDb(act SyncFileAction) *FileActionTask {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	if act == SyncFileActionDownload {
		if files, e := f.task.syncFileDb.GetFileList(SyncFileStatusDownloading); e == nil {
			for _, file := range files {
				if !f.fileInProcessQueue.Contains(file) {
					return &FileActionTask{
						localFileDb:          f.task.localFileDb,
						panFileDb:            f.task.panFileDb,
						syncFileDb:           f.task.syncFileDb,
						panClient:            f.task.panClient,
						syncItem:             file,
						maxDownloadRate:      f.maxDownloadRate,
						maxUploadRate:        f.maxUploadRate,
						panFolderCreateMutex: f.folderCreateMutex,
					}
				}
			}
		}
	} else if act == SyncFileActionUpload {
		if files, e := f.task.syncFileDb.GetFileList(SyncFileStatusUploading); e == nil {
			for _, file := range files {
				if !f.fileInProcessQueue.Contains(file) {
					return &FileActionTask{
						localFileDb:          f.task.localFileDb,
						panFileDb:            f.task.panFileDb,
						syncFileDb:           f.task.syncFileDb,
						panClient:            f.task.panClient,
						syncItem:             file,
						maxDownloadRate:      f.maxDownloadRate,
						maxUploadRate:        f.maxUploadRate,
						panFolderCreateMutex: f.folderCreateMutex,
					}
				}
			}
		}
	}

	if files, e := f.task.syncFileDb.GetFileList(SyncFileStatusCreate); e == nil {
		if len(files) > 0 {
			for _, file := range files {
				if file.Action == act && !f.fileInProcessQueue.Contains(file) {
					return &FileActionTask{
						localFileDb:          f.task.localFileDb,
						panFileDb:            f.task.panFileDb,
						syncFileDb:           f.task.syncFileDb,
						panClient:            f.task.panClient,
						syncItem:             file,
						maxDownloadRate:      f.maxDownloadRate,
						maxUploadRate:        f.maxUploadRate,
						panFolderCreateMutex: f.folderCreateMutex,
					}
				}
			}
		}
	}
	return nil
}

// cleanSyncDbRecords 清楚同步数据库无用数据
func (f *FileActionTaskManager) cleanSyncDbRecords(ctx context.Context) {
	// TODO: failed / success / illegal
}

// fileActionTaskExecutor 异步执行文件操作
func (f *FileActionTaskManager) fileActionTaskExecutor(ctx context.Context) {
	f.wg.AddDelta()
	defer f.wg.Done()

	downloadWaitGroup := waitgroup.NewWaitGroup(f.fileDownloadParallel)
	uploadWaitGroup := waitgroup.NewWaitGroup(f.fileUploadParallel)
	deleteLocalWaitGroup := waitgroup.NewWaitGroup(1)
	deletePanWaitGroup := waitgroup.NewWaitGroup(1)

	for {
		select {
		case <-ctx.Done():
			// cancel routine & done
			logger.Verboseln("file executor routine done")
			downloadWaitGroup.Wait()
			return
		default:
			//logger.Verboseln("do file executor process")
			if f.getSyncActionModifyCount() <= 0 {
				time.Sleep(1 * time.Second)
				continue
			}

			actionIsEmptyOfThisTerm := true
			// do upload
			uploadItem := f.getFromSyncDb(SyncFileActionUpload)
			if uploadItem != nil {
				actionIsEmptyOfThisTerm = false
				if uploadWaitGroup.Parallel() < f.fileUploadParallel {
					uploadWaitGroup.AddDelta()
					f.fileInProcessQueue.PushUnique(uploadItem.syncItem)
					go func() {
						if e := uploadItem.DoAction(ctx); e == nil {
							// success
							f.fileInProcessQueue.Remove(uploadItem.syncItem)
						} else {
							// retry?
							f.fileInProcessQueue.Remove(uploadItem.syncItem)
						}
						uploadWaitGroup.Done()
					}()
				}
			}

			// do download
			downloadItem := f.getFromSyncDb(SyncFileActionDownload)
			if downloadItem != nil {
				actionIsEmptyOfThisTerm = false
				if downloadWaitGroup.Parallel() < f.fileDownloadParallel {
					downloadWaitGroup.AddDelta()
					f.fileInProcessQueue.PushUnique(downloadItem.syncItem)
					go func() {
						if e := downloadItem.DoAction(ctx); e == nil {
							// success
							f.fileInProcessQueue.Remove(downloadItem.syncItem)
						} else {
							// retry?
							f.fileInProcessQueue.Remove(downloadItem.syncItem)
						}
						downloadWaitGroup.Done()
					}()
				}
			}

			// delete local
			deleteLocalItem := f.getFromSyncDb(SyncFileActionDeleteLocal)
			if deleteLocalItem != nil {
				actionIsEmptyOfThisTerm = false
				if deleteLocalWaitGroup.Parallel() < 1 {
					deleteLocalWaitGroup.AddDelta()
					f.fileInProcessQueue.PushUnique(deleteLocalItem.syncItem)
					go func() {
						if e := deleteLocalItem.DoAction(ctx); e == nil {
							// success
							f.fileInProcessQueue.Remove(deleteLocalItem.syncItem)
						} else {
							// retry?
							f.fileInProcessQueue.Remove(deleteLocalItem.syncItem)
						}
						deleteLocalWaitGroup.Done()
					}()
				}
			}

			// delete pan
			deletePanItem := f.getFromSyncDb(SyncFileActionDeletePan)
			if deletePanItem != nil {
				actionIsEmptyOfThisTerm = false
				if deletePanWaitGroup.Parallel() < 1 {
					deletePanWaitGroup.AddDelta()
					f.fileInProcessQueue.PushUnique(deletePanItem.syncItem)
					go func() {
						if e := deletePanItem.DoAction(ctx); e == nil {
							// success
							f.fileInProcessQueue.Remove(deletePanItem.syncItem)
						} else {
							// retry?
							f.fileInProcessQueue.Remove(deletePanItem.syncItem)
						}
						deletePanWaitGroup.Done()
					}()
				}
			}

			// check action list is empty or not
			if actionIsEmptyOfThisTerm {
				// all action queue is empty
				// complete one loop
				f.MinusSyncActionModifyCount()
			}

			// delay for next term
			time.Sleep(1 * time.Second)
		}
	}
}

// getRelativePath 获取文件的相对路径
func (l *localFileSet) getRelativePath(localPath string) string {
	localPath = strings.ReplaceAll(localPath, "\\", "/")
	localRootPath := strings.ReplaceAll(l.localFolderPath, "\\", "/")
	relativePath := strings.TrimPrefix(localPath, localRootPath)
	return path.Clean(relativePath)
}

// Intersection 交集
func (l *localFileSet) Intersection(other *panFileSet) (LocalFileList, PanFileList) {
	localFilePathSet := mapset.NewThreadUnsafeSet()
	relativePathLocalMap := map[string]*LocalFileItem{}
	for _, item := range l.items {
		rp := l.getRelativePath(item.Path)
		relativePathLocalMap[rp] = item
		localFilePathSet.Add(rp)
	}

	localFileList := LocalFileList{}
	panFileList := PanFileList{}
	for _, item := range other.items {
		rp := other.getRelativePath(item.Path)
		if localFilePathSet.Contains(rp) {
			localFileList = append(localFileList, relativePathLocalMap[rp])
			panFileList = append(panFileList, item)
		}
	}
	return localFileList, panFileList
}

// Difference 差集
func (l *localFileSet) Difference(other *panFileSet) LocalFileList {
	panFilePathSet := mapset.NewThreadUnsafeSet()
	for _, item := range other.items {
		rp := other.getRelativePath(item.Path)
		panFilePathSet.Add(rp)
	}

	localFileList := LocalFileList{}
	for _, item := range l.items {
		rp := l.getRelativePath(item.Path)
		if !panFilePathSet.Contains(rp) {
			localFileList = append(localFileList, item)
		}
	}
	return localFileList
}

// getRelativePath 获取文件的相对路径
func (p *panFileSet) getRelativePath(panPath string) string {
	panPath = strings.ReplaceAll(panPath, "\\", "/")
	panRootPath := strings.ReplaceAll(p.panFolderPath, "\\", "/")
	relativePath := strings.TrimPrefix(panPath, panRootPath)
	return path.Clean(relativePath)
}

// Intersection 交集
func (p *panFileSet) Intersection(other *localFileSet) (PanFileList, LocalFileList) {
	localFilePathSet := mapset.NewThreadUnsafeSet()
	relativePathLocalMap := map[string]*LocalFileItem{}
	for _, item := range other.items {
		rp := other.getRelativePath(item.Path)
		relativePathLocalMap[rp] = item
		localFilePathSet.Add(rp)
	}

	localFileList := LocalFileList{}
	panFileList := PanFileList{}
	for _, item := range p.items {
		rp := p.getRelativePath(item.Path)
		if localFilePathSet.Contains(rp) {
			localFileList = append(localFileList, relativePathLocalMap[rp])
			panFileList = append(panFileList, item)
		}
	}
	return panFileList, localFileList
}

// Difference 差集
func (p *panFileSet) Difference(other *localFileSet) PanFileList {
	localFilePathSet := mapset.NewThreadUnsafeSet()
	for _, item := range other.items {
		rp := other.getRelativePath(item.Path)
		localFilePathSet.Add(rp)
	}

	panFileList := PanFileList{}
	for _, item := range p.items {
		rp := p.getRelativePath(item.Path)
		if !localFilePathSet.Contains(rp) {
			panFileList = append(panFileList, item)
		}
	}
	return panFileList
}
